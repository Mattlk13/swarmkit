package container

import (
	"errors"
	"strings"

	engineapi "github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types/events"
	"github.com/docker/swarm-v2/agent/exec"
	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/log"
	"golang.org/x/net/context"
)

// Runner implements agent.Runner against docker's API.
//
// Most operations against docker's API are done through the container name,
// which is unique to the task.
type Runner struct {
	client     engineapi.APIClient
	task       *api.Task
	controller *containerController
	closed     chan struct{}
	err        error
}

var _ exec.Runner = &Runner{}

// NewRunner returns a dockerexec runner for the provided task.
func NewRunner(client engineapi.APIClient, task *api.Task) (*Runner, error) {
	ctrl, err := newContainerController(task)
	if err != nil {
		return nil, err
	}

	return &Runner{
		client:     client,
		task:       task,
		controller: ctrl,
		closed:     make(chan struct{}),
	}, nil
}

// Update tasks a recent task update and applies it to the container.
func (r *Runner) Update(ctx context.Context, t *api.Task) error {
	log.G(ctx).Warnf("task updates not yet supported")
	// TODO(stevvooe): While assignment of tasks is idempotent, we do allow
	// updates of metadata, such as labelling, as well as any other properties
	// that make sense.
	return nil
}

// Prepare creates a container and ensures the image is pulled.
//
// If the container has already be created, exec.ErrTaskPrepared is returned.
func (r *Runner) Prepare(ctx context.Context) error {
	for {
		if err := r.checkClosed(); err != nil {
			return err
		}

		if err := r.controller.create(ctx, r.client); err != nil {
			if isContainerCreateNameConflict(err) {
				if _, err := r.controller.inspect(ctx, r.client); err != nil {
					return err
				}

				// container is already created. success!
				return exec.ErrTaskPrepared
			}

			if !engineapi.IsErrImageNotFound(err) {
				return err
			}

			if err := r.controller.pullImage(ctx, r.client); err != nil {
				return err
			}

			continue // retry to create the container
		}

		break
	}

	return nil
}

func isContainerCreateNameConflict(err error) bool {
	// TODO(stevvooe): Very fragile error reporting from daemon. Need better
	// errors in engineapi.
	return strings.Contains(err.Error(), "Conflict. The name")
}

// Start the container. An error will be returned if the container is already started.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	ctnr, err := r.controller.inspect(ctx, r.client)
	if err != nil {
		return err
	}

	// Detect whether the container has *ever* been started. If so, we don't
	// issue the start.
	//
	// TODO(stevvooe): This is very racy. While reading inspect, another could
	// start the process and we could end up starting it twice.
	if ctnr.State.Status != "created" {
		return exec.ErrTaskStarted
	}

	if err := r.controller.start(ctx, r.client); err != nil {
		return err
	}

	return nil
}

// Wait on the container to exit.
func (r *Runner) Wait(pctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	eventq, closed, err := r.controller.events(ctx, r.client)
	if err != nil {
		return err
	}

	for {
		select {
		case event := <-eventq:
			log.G(ctx).Debugf("%v", event)

			if !r.matchevent(event) {
				continue
			}

			switch event.Action {
			case "die": // exit on terminal events
				ctnr, err := r.controller.inspect(ctx, r.client)
				if err != nil {
					return err
				}

				if ctnr.State.ExitCode != 0 {
					var cause error
					if ctnr.State.Error != "" {
						cause = errors.New(ctnr.State.Error)
					}

					return &exec.ExitError{
						Code:  ctnr.State.ExitCode,
						Cause: cause,
					}
				}

				return nil
			case "destroy":
				// If we get here, something has gone wrong but we want to exit
				// and report anyways.
				return ErrContainerDestroyed
			}
		case <-closed:
			// restart!
			eventq, closed, err = r.controller.events(ctx, r.client)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closed:
			return r.err
		}
	}
}

// Shutdown the container cleanly.
func (r *Runner) Shutdown(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	return r.controller.shutdown(ctx, r.client)
}

// Terminate the container, with force.
func (r *Runner) Terminate(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	return r.controller.terminate(ctx, r.client)
}

// Remove the container and its resources.
func (r *Runner) Remove(ctx context.Context) error {
	if err := r.checkClosed(); err != nil {
		return err
	}

	return r.controller.remove(ctx, r.client)
}

// Close the runner and clean up any ephemeral resources.
func (r *Runner) Close() error {
	select {
	case <-r.closed:
		return r.err
	default:
		r.err = exec.ErrRunnerClosed
		close(r.closed)
	}
	return nil
}

func (r *Runner) matchevent(event events.Message) bool {
	if event.Type != events.ContainerEventType {
		return false
	}

	// TODO(stevvooe): Filter based on ID matching, in addition to name.

	// Make sure the events are for this container.
	if event.Actor.Attributes["name"] != r.controller.container.name() {
		return false
	}

	return true
}

func (r *Runner) checkClosed() error {
	select {
	case <-r.closed:
		return r.err
	default:
		return nil
	}
}