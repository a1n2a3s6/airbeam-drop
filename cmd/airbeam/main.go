package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/a1n2a3s6/airbeam-drop/internal/netutil"
	"github.com/a1n2a3s6/airbeam-drop/internal/qr"
	"github.com/a1n2a3s6/airbeam-drop/internal/server"
)

const version = "1.0.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "airbeam: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	defaultDir := defaultSaveDir()

	// Allow flags to appear before or after the "send" subcommand and file paths.
	flagArgs, positional := splitFlagsAndPositional(args)

	fs := flag.NewFlagSet("airbeam", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	portFlag := fs.Int("port", 8765, "Preferred TCP port (auto-picks a free port if busy)")
	dirFlag := fs.String("dir", defaultDir, "Output directory for received files")
	noPinFlag := fs.Bool("no-pin", false, "Disable 4-digit security PIN")
	showVersion := fs.Bool("version", false, "Print version and exit")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `AirBeam v%s — Zero-app LAN file & clipboard beam

Usage:
  airbeam [flags]
  airbeam send <file1> [file2 ...] [flags]

Flags:
`, version)
		fs.PrintDefaults()
	}

	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		fmt.Printf("airbeam v%s\n", version)
		return nil
	}

	var sendFiles []string
	if len(positional) > 0 {
		if positional[0] == "send" {
			sendFiles = positional[1:]
			if len(sendFiles) == 0 {
				return errors.New("usage: airbeam send <file1> [file2 ...]")
			}
		} else {
			// Allow passing files directly: `airbeam photo.png build.zip`
			sendFiles = positional
		}
	}

	saveDir, err := expandPath(*dirFlag)
	if err != nil {
		return fmt.Errorf("invalid --dir path: %w", err)
	}

	lanIP, ipErr := netutil.DetectLANIP()
	ln, boundPort, err := netutil.ListenAvailable(*portFlag)
	if err != nil {
		return err
	}
	defer ln.Close()

	pin := ""
	if !*noPinFlag {
		pin = server.GeneratePIN()
	}

	srv, err := server.New(server.Config{
		Host:    lanIP,
		Port:    boundPort,
		SaveDir: saveDir,
		PIN:     pin,
		Logger: func(format string, a ...any) {
			ts := time.Now().Format("15:04:05")
			fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, a...))
		},
	})
	if err != nil {
		return err
	}

	var preloaded []server.FileEntry
	for _, fpath := range sendFiles {
		expanded, err := expandPath(fpath)
		if err != nil {
			return err
		}
		entry, err := srv.AddLocalFile(expanded)
		if err != nil {
			return err
		}
		preloaded = append(preloaded, entry)
	}

	printBanner(lanIP, boundPort, *portFlag, pin, saveDir, srv.ConnectURL(), ipErr, preloaded)

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// ReadTimeout & WriteTimeout intentionally left 0 to support multi-GB streaming uploads/downloads.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		fmt.Println("\nShutting down AirBeam...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		return serveErr
	}
}

func printBanner(ip string, port, preferredPort int, pin, saveDir, connectURL string, ipErr error, preloaded []server.FileEntry) {
	fmt.Println()
	fmt.Printf("  AIRBEAM LAN v%s  //  ZERO-APP WIFI TRANSFER\n", version)
	fmt.Println("  ─────────────────────────────────────────────────────")

	if qrArt, err := qr.RenderHalfBlock(connectURL); err == nil {
		for _, line := range strings.Split(qrArt, "\n") {
			fmt.Printf("  %s\n", line)
		}
		fmt.Println()
	}

	fmt.Printf("  LAN URL   : http://%s:%d\n", ip, port)
	if port != preferredPort {
		fmt.Printf("  PORT NOTE : %d was busy; auto-selected %d\n", preferredPort, port)
	}
	if pin != "" {
		fmt.Printf("  PIN CODE  : %s  (auto-filled when scanning QR)\n", pin)
	} else {
		fmt.Println("  PIN CODE  : DISABLED (--no-pin)")
	}
	fmt.Printf("  SAVE DIR  : %s\n", saveDir)
	if ipErr != nil {
		fmt.Printf("  WARNING   : %v\n", ipErr)
	}

	if len(preloaded) > 0 {
		fmt.Println("  READY ON PC:")
		for _, f := range preloaded {
			fmt.Printf("    • %s (%s)\n", f.Name, f.SizeHuman)
		}
	}

	fmt.Println("  ─────────────────────────────────────────────────────")
	fmt.Println("  Scan QR with your phone camera · Press Ctrl+C to stop")
	fmt.Println()
}

func defaultSaveDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "AirBeam"
	}
	return filepath.Join(home, "Downloads", "AirBeam")
}

func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// splitFlagsAndPositional separates known flags from positional args so users can write
// either `airbeam --port 9000 send a.zip` or `airbeam send a.zip --port 9000`.
func splitFlagsAndPositional(args []string) (flags []string, positional []string) {
	valueFlags := map[string]bool{
		"-port": true, "--port": true,
		"-dir": true, "--dir": true,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return flags, positional
}
