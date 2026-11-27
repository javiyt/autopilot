package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/javimartin/pi-autopilot/internal/autopilot"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: pi-autopilot <run|triage|status|doctor|discover-chat|service|bump> -config /path/config.yaml [options]")
	fmt.Fprintln(os.Stderr, "  run: optional -once to process one issue then exit")
	fmt.Fprintln(os.Stderr, "  triage: plan the issue queue once and print the order without executing")
	fmt.Fprintln(os.Stderr, "  status: show durable state without contacting Pi or GitHub")
	fmt.Fprintln(os.Stderr, "  doctor: check local binaries and registered Pi models")
	fmt.Fprintln(os.Stderr, "  discover-chat: show Telegram chat IDs after sending /start to your bot")
	fmt.Fprintln(os.Stderr, "  service <install|uninstall|start|stop|status>: manage the user-level system service (launchd or systemd user)")
	fmt.Fprintln(os.Stderr, "  bump: bump SemVer version for new main commit")
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	action := os.Args[1]
	flags := flag.NewFlagSet(action, flag.ExitOnError)
	configPath := flags.String("config", "config.yaml", "YAML configuration")
	once := flags.Bool("once", false, "Stop after the active issue is merged (run only)")
	if err := flags.Parse(os.Args[2:]); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	token := os.Getenv("PI_AUTOPILOT_TELEGRAM_TOKEN")

	if action == "discover-chat" {
		if token == "" {
			log.Fatal("PI_AUTOPILOT_TELEGRAM_TOKEN is not configured")
		}
		t := autopilot.NewTelegram(token, 0)
		ids, err := t.DiscoverChatIDs(ctx)
		if err != nil {
			log.Fatal(err)
		}
		if len(ids) == 0 {
			fmt.Println("No messages yet: send /start to your own Telegram bot and retry.")
			return
		}
		fmt.Println("Chat IDs seen by your bot (copy only your own/private chat ID into config.yaml):")
		for _, id := range ids {
			fmt.Println(id)
		}
		return
	}

	if action == "service" {
		sub := "help"
		if len(os.Args) > 2 && !strings.HasPrefix(os.Args[2], "-") {
			sub = os.Args[2]
		}
		var (
			out string
			err error
		)
		switch sub {
		case "install":
			if token == "" {
				log.Fatal("PI_AUTOPILOT_TELEGRAM_TOKEN is required for service install; the unit stores it for unattended runs")
			}
			absolutePath, err := filepath.Abs(*configPath)
			if err != nil {
				log.Fatal(err)
			}
			cfg, err := autopilot.ReadConfig(absolutePath)
			if err != nil {
				log.Fatal(err)
			}
			out, err = autopilot.ServiceInstall(cfg, token)
		case "uninstall":
			out, err = autopilot.ServiceUninstall()
		case "start":
			out, err = autopilot.ServiceStart()
		case "stop":
			out, err = autopilot.ServiceStop()
		case "status", "":
			out, err = autopilot.ServiceStatus()
		default:
			usage()
			os.Exit(2)
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(out)
		return
	}

	absolutePath, err := filepath.Abs(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := autopilot.ReadConfig(absolutePath)
	if err != nil {
		log.Fatal(err)
	}

	switch action {
	case "triage":
		app, err := autopilot.NewApp(cfg, token)
		if err != nil {
			log.Fatal(err)
		}
		defer app.Close()
		report, err := app.PlanTasks(ctx)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(report)
	case "status":
		state, err := autopilot.LoadState(filepath.Join(cfg.StateDir, "state.json"), cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Repository: %s\nIssue: #%d %s\nStage: %s\nPaused: %t\nError: %s\nUpdated: %s\n",
			cfg.Repo, state.Issue.Number, state.Issue.Title, state.Phase, state.Paused, state.LastError, state.UpdatedAt.Format(time.RFC3339))
	case "doctor":
		if token == "" {
			fmt.Println("WARNING: Telegram token is not set; interactive Telegram control is disabled.")
		}
		out, err := autopilot.Doctor(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.TrimSpace(out))
	case "run":
		if token == "" {
			log.Fatal("PI_AUTOPILOT_TELEGRAM_TOKEN is required for supervised unattended mode")
		}
		app, err := autopilot.NewApp(cfg, token)
		if err != nil {
			log.Fatal(err)
		}
		defer app.Close()
		if err := app.Work(ctx, *once); err != nil && err != context.Canceled {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}
