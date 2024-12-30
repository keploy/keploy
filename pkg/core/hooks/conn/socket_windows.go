//go:build windows

package conn

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
)

var eventAttributesSize = int(unsafe.Sizeof(SocketDataEvent{}))

// ListenSocket starts the socket event listeners
func ListenSocket(ctx context.Context, l *zap.Logger, openEventChan chan SocketOpenEvent, dataEventChan chan SocketDataEvent, closeEventChan chan SocketCloseEvent, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	t := make(chan *models.TestCase, 500)
	c := NewFactory(time.Minute, l)
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// TODO refactor this to directly consume the events from the maps
					c.ProcessActiveTrackers(ctx, t, opts)
					time.Sleep(100 * time.Millisecond)
				}
			}
		}()
		<-ctx.Done()
		close(t)
		return nil
	})

	err := open(ctx, c, l, openEventChan)
	if err != nil {
		utils.LogError(l, err, "failed to start open socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	err = data(ctx, c, l, dataEventChan)
	if err != nil {
		utils.LogError(l, err, "failed to start data socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	err = exit(ctx, c, l, closeEventChan)
	if err != nil {
		utils.LogError(l, err, "failed to start close socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	return t, err
}

func open(ctx context.Context, c *Factory, l *zap.Logger, m chan SocketOpenEvent) error {
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				var event SocketOpenEvent
				event = <-m
				c.GetOrCreate(event.ConnID).AddOpenEvent(event)
			}
		}()
		<-ctx.Done() // Check for context cancellation
		return nil
	})
	return nil
}

func data(ctx context.Context, c *Factory, l *zap.Logger, m chan SocketDataEvent) error {
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				var event SocketDataEvent
				event = <-m
				c.GetOrCreate(event.ConnID).AddDataEvent(event)
			}
		}()
		<-ctx.Done() // Check for context cancellation
		return nil
	})
	return nil
}

func exit(ctx context.Context, c *Factory, l *zap.Logger, m chan SocketCloseEvent) error {
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				var event SocketCloseEvent
				event = <-m
				c.GetOrCreate(event.ConnID).AddCloseEvent(event)
			}
		}()
		<-ctx.Done() // Check for context cancellation
		return nil
	})
	return nil
}
