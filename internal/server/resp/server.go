// Copyright (c) 2022-present, DiceDB contributors
// All rights reserved. Licensed under the BSD 3-Clause License. See LICENSE file in the project root for full license information.

package resp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dicedb/dice/internal/commandhandler"
	"github.com/dicedb/dice/internal/ops"
	"github.com/dicedb/dice/internal/server/abstractserver"
	"github.com/dicedb/dice/internal/wal"

	dstore "github.com/dicedb/dice/internal/store"
	"github.com/dicedb/dice/internal/watchmanager"

	"github.com/dicedb/dice/config"
	"github.com/dicedb/dice/internal/clientio/iohandler/netconn"
	respparser "github.com/dicedb/dice/internal/clientio/requestparser/resp"
	"github.com/dicedb/dice/internal/iothread"
	"github.com/dicedb/dice/internal/shard"
)

var (
	ioThreadCounter   uint64
	cmdHandlerCounter uint64
	startTime         = time.Now().UnixNano() / int64(time.Millisecond)
)

var (
	ErrInvalidIPAddress = errors.New("invalid IP address")
)

const (
	DefaultConnBacklogSize = 128
)

type Server struct {
	abstractserver.AbstractServer
	Host                     string
	Port                     int
	serverFD                 int
	connBacklogSize          int
	ioThreadManager          *iothread.Manager
	cmdHandlerManager        *commandhandler.Registry
	shardManager             *shard.ShardManager
	watchManager             *watchmanager.Manager
	cmdWatchSubscriptionChan chan watchmanager.WatchSubscription
	globalErrorChan          chan error
	wl                       wal.AbstractWAL
}

func NewServer(shardManager *shard.ShardManager, ioThreadManager *iothread.Manager, cmdHandlerManager *commandhandler.Registry,
	cmdWatchSubscriptionChan chan watchmanager.WatchSubscription, cmdWatchChan chan dstore.CmdWatchEvent,
	globalErrChan chan error, wl wal.AbstractWAL) *Server {
	return &Server{
		Host:                     config.Config.Host,
		Port:                     config.Config.Port,
		connBacklogSize:          DefaultConnBacklogSize,
		ioThreadManager:          ioThreadManager,
		cmdHandlerManager:        cmdHandlerManager,
		shardManager:             shardManager,
		watchManager:             watchmanager.NewManager(cmdWatchSubscriptionChan, cmdWatchChan),
		cmdWatchSubscriptionChan: cmdWatchSubscriptionChan,
		globalErrorChan:          globalErrChan,
		wl:                       wl,
	}
}

func (s *Server) Run(ctx context.Context) (err error) {
	// BindAndListen the desired port to the server
	if err = s.BindAndListen(); err != nil {
		slog.Error("failed to bind server", slog.Any("error", err))
		return err
	}

	defer s.ReleasePort()

	// Start a go routine to accept connections
	errChan := make(chan error, 1)
	wg := &sync.WaitGroup{}

	if s.cmdWatchSubscriptionChan != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watchManager.Run(ctx)
		}()
	}

	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		if err := s.AcceptConnectionRequests(ctx, wg); err != nil {
			errChan <- fmt.Errorf("failed to accept connections %w", err)
		}
	}(wg)

	select {
	case <-ctx.Done():
		slog.Info("initiating shutdown")
	case err = <-errChan:
		slog.Error("error while accepting connections, initiating shutdown", slog.Any("error", err))
	}

	s.Shutdown()

	wg.Wait() // Wait for the go routines to finish
	slog.Info("exiting gracefully")

	return err
}

func (s *Server) BindAndListen() error {
	serverFD, socketErr := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if socketErr != nil {
		return fmt.Errorf("failed to create socket: %w", socketErr)
	}

	// Close the socket on exit if an error occurs
	var err error
	defer func() {
		if err != nil {
			if closeErr := syscall.Close(serverFD); closeErr != nil {
				// Wrap the close error with the original bind/listen error
				slog.Error("Error occurred", slog.Any("error", err), "additionally, failed to close socket", slog.Any("close-err", closeErr))
			} else {
				slog.Error("Error occurred", slog.Any("error", err))
			}
		}
	}()

	if err = syscall.SetsockoptInt(serverFD, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("failed to set SO_REUSEADDR: %w", err)
	}

	if err = syscall.SetNonblock(serverFD, true); err != nil {
		return fmt.Errorf("failed to set socket to non-blocking: %w", err)
	}

	ip4 := net.ParseIP(s.Host)
	if ip4 == nil {
		return ErrInvalidIPAddress
	}

	sockAddr := &syscall.SockaddrInet4{
		Port: s.Port,
		Addr: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
	}
	if err = syscall.Bind(serverFD, sockAddr); err != nil {
		return fmt.Errorf("failed to bind socket: %w", err)
	}

	if err = syscall.Listen(serverFD, s.connBacklogSize); err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}

	s.serverFD = serverFD
	return nil
}

// ReleasePort closes the server socket.
func (s *Server) ReleasePort() {
	if err := syscall.Close(s.serverFD); err != nil {
		slog.Error("Failed to close server socket", slog.Any("error", err))
	}
}

// AcceptConnectionRequests accepts new client connections
func (s *Server) AcceptConnectionRequests(ctx context.Context, wg *sync.WaitGroup) error {
	for {
		select {
		case <-ctx.Done():
			slog.Info("no new connections will be accepted")

			return ctx.Err()
		default:
			clientFD, _, err := syscall.Accept(s.serverFD)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
					continue // No more connections to accept at this time
				}

				return fmt.Errorf("error accepting connection: %w", err)
			}

			// Register a new io-thread for the client
			ioHandler, err := netconn.NewIOHandler(clientFD)
			if err != nil {
				slog.Error("Failed to create new IOHandler for clientFD", slog.Int("client-fd", clientFD), slog.Any("error", err))
				return err
			}

			// create a new io-thread
			ioThreadID := GenerateUniqueIOThreadID()
			ioThreadReadChan := make(chan []byte)       // for sending data to the command handler from the io-thread
			ioThreadWriteChan := make(chan interface{}) // for sending data to the io-thread from the command handler
			ioThreadErrChan := make(chan error, 1)      // for receiving errors from the io-thread
			thread := iothread.NewIOThread(ioThreadID, ioHandler, ioThreadReadChan, ioThreadWriteChan, ioThreadErrChan)

			// For each io-thread, we create a dedicated command handler - 1:1 mapping
			cmdHandlerID := GenerateUniqueCommandHandlerID()
			parser := respparser.NewParser()
			responseChan := make(chan *ops.StoreResponse)      // responseChan is used for handling common responses from shards
			preprocessingChan := make(chan *ops.StoreResponse) // preprocessingChan is specifically for handling responses from shards for commands that require preprocessing

			handler := commandhandler.NewCommandHandler(cmdHandlerID, responseChan, preprocessingChan,
				s.cmdWatchSubscriptionChan, parser, s.shardManager, s.globalErrorChan,
				ioThreadReadChan, ioThreadWriteChan, ioThreadErrChan, s.wl)

			// Register the io-thread with the manager
			err = s.ioThreadManager.RegisterIOThread(thread)
			if err != nil {
				slog.Debug("Failed to register io-thread", slog.String("id", ioThreadID), slog.Any("error", err))
				continue
			}

			// Register the command handler with the manager
			err = s.cmdHandlerManager.RegisterCommandHandler(handler)
			if err != nil {
				slog.Debug("Failed to register command handler", slog.String("id", cmdHandlerID), slog.Any("error", err))
				continue
			}

			// Registration for both IO thread and command handler is done to ensure there is no error before starting the goroutines
			wg.Add(2)
			go s.startIOThread(ctx, wg, thread)
			go s.startCommandHandler(ctx, wg, handler)
		}
	}
}

func (s *Server) startIOThread(ctx context.Context, wg *sync.WaitGroup, thread *iothread.BaseIOThread) {
	wg.Done()
	defer func(wm *iothread.Manager, id string) {
		err := wm.UnregisterIOThread(id)
		if err != nil {
			slog.Warn("Failed to unregister io-thread", slog.String("id", id), slog.Any("error", err))
		}
	}(s.ioThreadManager, thread.ID())
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	err := thread.Start(ctx2)
	if err != nil {
		slog.Debug("IOThread stopped", slog.String("id", thread.ID()), slog.Any("error", err))
	}
}

func (s *Server) startCommandHandler(ctx context.Context, wg *sync.WaitGroup, cmdHandler *commandhandler.BaseCommandHandler) {
	wg.Done()
	defer func(wm *commandhandler.Registry, id string) {
		err := wm.UnregisterCommandHandler(id)
		if err != nil {
			slog.Warn("Failed to unregister command handler", slog.String("id", id), slog.Any("error", err))
		}
	}(s.cmdHandlerManager, cmdHandler.ID())
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	err := cmdHandler.Start(ctx2)
	if err != nil {
		slog.Debug("CommandHandler stopped", slog.String("id", cmdHandler.ID()), slog.Any("error", err))
	}
}

func GenerateUniqueIOThreadID() string {
	return GenerateUniqueID("I", &ioThreadCounter)
}

func GenerateUniqueCommandHandlerID() string {
	return GenerateUniqueID("C", &cmdHandlerCounter)
}

func GenerateUniqueID(prefix string, counter *uint64) string {
	count := atomic.AddUint64(counter, 1)
	timestamp := time.Now().UnixMilli() - startTime
	return fmt.Sprintf("%s-%d-%d", prefix, timestamp, count)
}

func (s *Server) Shutdown() {
	// Not implemented
}
