package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/worker"
)

func main() {
	path := flag.String("config", "/etc/model-gateway/bootstrap.json", "configuration center bootstrap file")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *path); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, path string) error {
	loader, err := config.NewLoader(path)
	if err != nil {
		return err
	}
	defer loader.Close()
	runtime, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	cfg := runtime.Current()
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	db, err := store.OpenDatabase(dbCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	ledger := store.New(db)
	app := httpapi.New(ledger, runtime)
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		return err
	}
	// 先排空 HTTP，再等后台写入完成，最后关闭两者共享的 PG 连接。
	defer reconciler.Close()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	failures := make(chan error, 1)
	serve := func(listener net.Listener) *server.Hertz {
		server := app.HTTPServer(listener)
		go func() {
			if err := server.Run(); err != nil {
				select {
				case failures <- errors.New("HTTP server stopped unexpectedly"):
				default:
				}
			}
		}()
		return server
	}
	server := serve(listener)
	reconciler.Start()
	var retiring sync.WaitGroup
	defer func() {
		shutdown(server)
		retiring.Wait()
	}()
	stopListening, err := loader.Listen(ctx, runtime, func(listener net.Listener) {
		old := server
		server = serve(listener)
		// 旧监听只排空 HTTP，后台已受理任务继续由同一网关执行。
		retiring.Add(1)
		go func() { defer retiring.Done(); shutdown(old) }()
	})
	if err != nil {
		return err
	}
	defer stopListening()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failures:
		return err
	}
}

func shutdown(server *server.Hertz) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if server.Shutdown(ctx) != nil {
		_ = server.Close()
	}
}
