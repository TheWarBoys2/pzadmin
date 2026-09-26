// Command pzadmin is a self-hosted administration console for Project Zomboid
// dedicated servers.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// Embedding the timezone database means the binary resolves IANA names on
	// its own. Without it a scratch-based image would silently fall back to
	// UTC, and every schedule would fire at the wrong hour half the year.
	_ "time/tzdata"

	"github.com/TheWarBoys2/pzadmin/internal/server"
)

// defaultGameImage is the game server image new servers are created with when
// PZADMIN_GAME_IMAGE is not set. It is a version tag, not latest, so every
// server made from one PZAdmin release starts from the same image. Bump it
// in a release after checking the new image works.
const defaultGameImage = "indifferentbroccoli/projectzomboid-server-docker:v1.1.9"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("pzadmin ")

	// The container image has no shell, so the Docker healthcheck runs the
	// binary itself in this mode.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	dataDir := env("PZADMIN_DATA", "/data")
	addr := env("PZADMIN_ADDR", ":27815")
	// A Project Zomboid installation shared by every server, used only when a
	// server has none of its own.
	gameRoot := env("PZADMIN_GAME_ROOT", "")

	// The two managed folders, mounted at the same path inside this
	// container as on the host, so a path written in a compose file opens
	// here as written.
	dataRoot := env("PZADMIN_DATA_ROOT", "")
	stacksRoot := env("PZADMIN_STACKS_ROOT", "")
	for _, root := range []struct{ name, path string }{
		{"PZADMIN_DATA_ROOT", dataRoot},
		{"PZADMIN_STACKS_ROOT", stacksRoot},
	} {
		warning, err := checkRoot(root.path)
		if err != nil {
			log.Fatalf("%s: %v", root.name, err)
		}
		if warning != "" {
			log.Printf("%s: %s", root.name, warning)
		}
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("cannot use data directory %s: %v", dataDir, err)
	}
	// A read-only or unwritable data directory is the single most common
	// installation mistake, and it fails in confusing ways later. Fail now.
	if err := checkDataDir(dataDir); err != nil {
		log.Fatalf("%v. It belongs to another user, usually because the user: line in docker-compose.yml "+
			"changed after the data volume was made. Give it to this user: find the volume's name "+
			"with docker volume ls (it ends in pzadmin-data, for example myfolder_pzadmin-data), then run "+
			"docker run --rm -v <that name>:/data busybox chown -R %d:%d /data",
			err, os.Getuid(), os.Getgid())
	}

	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("embedded assets are missing: %v", err)
	}

	app, err := server.New(server.Options{
		DataDir:      dataDir,
		Assets:       assets,
		GameRoot:     gameRoot,
		DataRoot:     dataRoot,
		StacksRoot:   stacksRoot,
		RCONHost:     env("PZADMIN_RCON_HOST", "host.docker.internal"),
		GameImage:    env("PZADMIN_GAME_IMAGE", defaultGameImage),
		BackupDir:    env("PZADMIN_BACKUP_DIR", ""),
		ArcaneURL:    env("PZADMIN_ARCANE_URL", ""),
		ArcaneEnvID:  env("PZADMIN_ARCANE_ENV_ID", ""),
		ArcaneAPIKey: env("PZADMIN_ARCANE_API_KEY", ""),

		TrustedProxies: env("PZADMIN_TRUSTED_PROXIES", ""),
	})
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}
	app.Start()

	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout covers reading the request, body included, so a client
		// cannot hold a connection open by sending its body a byte at a time.
		// It does not limit the response, so the event streams are unaffected:
		// net/http clears the read deadline once the body has been read.
		ReadTimeout: 60 * time.Second,
		// No WriteTimeout: the event stream is a long-lived response and any
		// deadline here would cut it off mid-flight.
		IdleTimeout: 120 * time.Second,
	}

	// Open event streams never finish on their own, and Shutdown waits for
	// every handler, so end them the moment shutdown starts. Otherwise one
	// open browser tab holds shutdown past Docker's stop timeout and the
	// final save never runs.
	srv.RegisterOnShutdown(app.BeginShutdown)

	go func() {
		log.Printf("version %s listening on %s (data %s, timezone %s)",
			server.Version, addr, dataDir, time.Now().Format("MST-0700"))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	app.Close()
	log.Print("stopped")
}

// healthcheck probes the local listener and returns a process exit code.
func healthcheck() int {
	addr := env("PZADMIN_ADDR", ":27815")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// checkRoot refuses a managed folder that is unset, relative or missing.
//
// An empty folder is only a warning. It is what a brand new install looks
// like, before the first server is created, but it is also what a mistyped
// path looks like, because Docker creates a missing bind source as an empty
// directory. So PZAdmin starts, and the log says which of the two to check.
func checkRoot(path string) (warning string, err error) {
	if path == "" {
		return "", errors.New("not set. It must be the absolute path of the folder, mounted at the same path")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not an absolute path", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s is not mounted into the container: %v", path, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %v", path, err)
	}
	defer f.Close()
	if names, _ := f.Readdirnames(1); len(names) == 0 {
		return fmt.Sprintf("%s is empty. That is fine for a new install; create your first server "+
			"from the dashboard. If you already have servers, check this path in docker-compose.yml "+
			"against the real folder on the host.", path), nil
	}
	return "", nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// checkDataDir makes sure this user can write the data folder and every file
// already in it. The image's /data lets any user create files, so after a
// change of user: the folder itself is writable while the files the old user
// made are not; checking the folder alone would miss that.
func checkDataDir(dir string) error {
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("data directory %s is not writable by user %d:%d: %w", dir, os.Getuid(), os.Getgid(), err)
	}
	_ = os.Remove(probe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cannot read data directory %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, e.Name()), os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("%s in the data directory is not writable by user %d:%d: %w",
				e.Name(), os.Getuid(), os.Getgid(), err)
		}
		_ = f.Close()
	}
	return nil
}
