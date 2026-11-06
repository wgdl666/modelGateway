package main

import (
	"bytes"
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
	"github.com/wgdl666/modelGateway/internal/gateway"
)

func main() {
	path := flag.String("config", "/etc/model-gateway/bootstrap.json", "configuration center bootstrap file")
	migrate := flag.Bool("migrate", false, "initialize Ent schema explicitly and exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *path, *migrate); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, path string, migration bool) error {
	loader := config.NewLoader(path)
	defer loader.Close()
	raw, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	cfg, err := gateway.ParseConfig(raw)
	if err != nil {
		return err
	}
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	db, err := gateway.OpenDatabase(dbCtx, cfg.DatabaseURL, migration)
	cancel()
	if err != nil {
		return err
	}
	if migration {
		defer db.Close()
		slog.Info("schema initialized with Ent")
		return nil
	}
	app, err := gateway.NewServer(&gateway.Store{DB: db}, cfg)
	if err != nil {
		_ = db.Close()
		return err
	}
	defer app.Close()
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
	app.StartReconciler()
	var retiring sync.WaitGroup
	defer func() {
		shutdown(server)
		retiring.Wait()
	}()
	// 两个配置中心共用整份 JSON 热加载；失败时不替换已应用快照，也不输出配置正文。
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case <-ticker.C:
			nextRaw, err := loader.Load(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("configuration reload failed", "reason", err.Error())
				}
				continue
			}
			if bytes.Equal(raw, nextRaw) {
				continue
			}
			next, err := gateway.ParseConfig(nextRaw)
			var nextListener net.Listener
			if err == nil && next.Listen != cfg.Listen {
				nextListener, err = net.Listen("tcp", next.Listen)
				if err != nil {
					err = errors.New("cannot listen on configured address")
				}
			}
			if err == nil {
				reloadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err = app.Reload(reloadCtx, next)
				cancel()
			}
			if err != nil {
				if nextListener != nil {
					_ = nextListener.Close()
				}
				slog.Error("configuration reload failed", "reason", err.Error())
				continue
			}
			if nextListener != nil {
				old := server
				server = serve(nextListener)
				// 新地址先接流量，旧监听只排空 HTTP；后台已受理任务仍由同一网关执行。
				retiring.Add(1)
				go func() { defer retiring.Done(); shutdown(old) }()
			}
			cfg, raw = next, nextRaw
			slog.Info("configuration reloaded")
		}
	}
}

func shutdown(server *server.Hertz) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if server.Shutdown(ctx) != nil {
		_ = server.Close()
	}
}
