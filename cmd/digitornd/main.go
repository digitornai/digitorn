// Command digitornd is the Digitorn daemon entrypoint. It runs either in the
// foreground (dev) or as a native OS service:
//
//	digitornd install | uninstall | start | stop | restart | status | run
//
// install bakes the absolute config path and working directory into the
// service definition so the service manager launches with the right context.
// Built-in modules advertise themselves via init() into module.Default;
// importing the package is enough to register them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kardianos/service"
	_ "go.uber.org/automaxprocs"

	"github.com/mbathepaul/digitorn/internal/config"
	"github.com/mbathepaul/digitorn/internal/server"
	"github.com/mbathepaul/digitorn/internal/version"

	_ "github.com/mbathepaul/digitorn/internal/modules/bash"
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
	_ "github.com/mbathepaul/digitorn/internal/modules/lsp"
	_ "github.com/mbathepaul/digitorn/internal/modules/web"
	_ "github.com/mbathepaul/digitorn/internal/modules/workspace"
)

type program struct {
	cfgPath string
	logger  service.Logger
	cancel  context.CancelFunc
	done    chan struct{}
}

func (p *program) Start(service.Service) error {
	cfg, err := config.Load(p.cfgPath)
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}
	d, err := server.Build(cfg)
	if err != nil {
		return fmt.Errorf("server build: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	go func() {
		defer close(p.done)
		if err := d.Start(ctx); err != nil && ctx.Err() == nil {
			_ = p.logger.Errorf("daemon stopped with error: %v", err)
		}
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done // Daemon.Shutdown is bounded by Server.ShutdownTimeout.
	}
	return nil
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("digitornd " + version.String())
		return
	}
	action := flag.Arg(0)

	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve config path: %v\n", err)
		os.Exit(1)
	}

	svcConfig := &service.Config{
		Name:             "digitornd",
		DisplayName:      "Digitorn Daemon",
		Description:      "Digitorn declarative AI agent daemon.",
		Arguments:        []string{"-config", absConfig, "run"},
		WorkingDirectory: filepath.Dir(absConfig),
		Option: service.KeyValue{
			"Restart":     "on-failure", // systemd
			"OnFailure":   "restart",    // Windows SCM
			"RunAtLoad":   true,         // launchd
			"LimitNOFILE": 1048576,      // systemd FD ceiling for concurrent connections
		},
	}

	prg := &program{cfgPath: absConfig}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service init: %v\n", err)
		os.Exit(1)
	}

	prg.logger, err = s.Logger(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service logger: %v\n", err)
		os.Exit(1)
	}

	switch action {
	case "", "run":
		if err := s.Run(); err != nil {
			_ = prg.logger.Error(err)
			os.Exit(1)
		}
	case "status":
		st, err := s.Status()
		if err != nil {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("digitornd: %s\n", statusText(st))
	case "install", "uninstall", "start", "stop", "restart":
		if err := service.Control(s, action); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
			os.Exit(1)
		}
		fmt.Printf("digitornd: %s ok\n", action)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (install|uninstall|start|stop|restart|status|run)\n", action)
		os.Exit(2)
	}
}

func statusText(st service.Status) string {
	switch st {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown (not installed?)"
	}
}
