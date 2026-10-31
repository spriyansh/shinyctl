/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/registry"
)

var detach bool

// handleHealthz answers once shinyctl-api is up, naming its process so that
// `serve -d` can tell its own child from a daemon that was already running.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.PIDHeader, strconv.Itoa(os.Getpid()))
	fmt.Fprintln(w, "ok")
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the shinyctl-api control-plane process",

	RunE: func(cmd *cobra.Command, args []string) error {
		if detach {
			exePath, err := os.Executable()
			if err != nil {
				return err
			}

			logPath, err := process.LogPath()
			if err != nil {
				return err
			}

			socketPath, err := process.SocketPath()
			if err != nil {
				return err
			}
			return newDetacher(exePath, socketPath, logPath).run()
		}

		// Daemon-side output goes through log, not fmt, so api.log carries
		// timestamps — "reconcile: Starting app" means nothing a week
		// later without knowing when.
		log.Println("shinyctl-api starting...")

		if warn := process.LingerWarning(); warn != "" {
			log.Print(warn)
		}

		socketPath, err := process.SocketPath()
		if err != nil {
			return err
		}

		// Both socket paths are checked before either is bound, so a home
		// directory that is too long fails here, with a message, and not
		// halfway through starting.
		if err := process.CheckSocketPaths(); err != nil {
			return err
		}
		proxyDir, err := process.ProxyDir()
		if err != nil {
			return err
		}

		listener, err := process.ListenUnix(socketPath)
		if err != nil {
			return err
		}
		defer listener.Close()

		// Before anything can call into it: the handlers and the loop
		// both read appRegistry, and the point of restoring it is that the
		// first tick already has every app the previous run was watching.
		registryPath, err := process.RegistryPath()
		if err != nil {
			return err
		}
		appRegistry, err = registry.Open(registryPath)
		if err != nil {
			return err
		}
		if n := len(appRegistry.All()); n > 0 {
			log.Printf("restored %d app(s) from %s", n, registryPath)
		}

		// The load balancer comes up with shinyctl-api, after the registry
		// is restored so its first sync already knows every app.
		exePath, err := os.Executable()
		if err != nil {
			return err
		}
		logPath, err := process.LogPath()
		if err != nil {
			return err
		}
		var sup *proxy.Supervisor
		gateway, sup, err = newGateway(proxyDir, exePath, logPath)
		if err != nil {
			return err
		}

		loopCtx, cancelLoop := context.WithCancel(context.Background())
		defer cancelLoop()
		sup.Expect()
		go sup.Run(loopCtx)

		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", handleHealthz)
		mux.HandleFunc("/apply", handleApply)
		mux.HandleFunc("/status", handleStatus)
		mux.HandleFunc("/stop", handleStop)
		mux.HandleFunc("/delete", handleDelete)
		mux.HandleFunc("/restart", handleRestart)

		server := &http.Server{Handler: mux}

		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("http server error: %v", err)
			}
		}()

		go runReconcileLoop(loopCtx)
		go runMemoryLoop(loopCtx, memoryInterval, process.ExactMemory)

		log.Printf("listening on %s, reconciling every %s", socketPath, reconcileInterval)

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		sig := <-sigs
		log.Printf("received %s, shutting down", sig)

		// Only shinyctl-api's own loops stop. The load balancer keeps
		// serving the apps, and the next shinyctl-api adopts it.
		cancelLoop()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		return server.Shutdown(ctx)
	},
}

func init() {
	serveCmd.Flags().BoolVarP(&detach, "detach", "d", false,
		"launch shinyctl-api in the background and return immediately")

	rootCmd.AddCommand(serveCmd)
}
