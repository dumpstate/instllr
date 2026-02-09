package main

import (
	"embed"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	cp "github.com/otiai10/copy"
	"github.com/urfave/cli/v2"
)

//go:embed templates
var templates embed.FS

type Service struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Tag   string `json:"tag"`
}

func (s *Service) String() string {
	return fmt.Sprintf("%s/%s:%s", s.Owner, s.Repo, s.Tag)
}

type Command int

const (
	Install Command = iota
	Uninstall
)

var (
	commands = map[string]Command{
		"install":   Install,
		"uninstall": Uninstall,
	}
)

func unsafeGet[T interface{}](value T, err error) T {
	if err != nil {
		log.Fatal(err)
	}

	return value
}

func unsafe(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func ensureUser(appName string) (int, int) {
	uid, err := id(appName, "-u")

	if err == nil {
		fmt.Printf("User '%s' already exists\n", appName)
		gid := unsafeGet(id(appName, "-g"))

		return uid, gid
	}

	cmd := exec.Command("useradd", "-mrU", appName)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	unsafe(cmd.Run())

	return unsafeGet(id(appName, "-u")), unsafeGet(id(appName, "-g"))
}

func systemdConf(name string) string {
	return fmt.Sprintf("/etc/systemd/system/%s.service", name)
}

func nginxConf(host string) string {
	return fmt.Sprintf("/etc/nginx/sites-enabled/%s.conf", host)
}

func serviceTemplate(deps *map[string]string, name string, runCmd []string, appEnv []string, targetDir string, uid int, gid int) {
	var run []string
	cmdPath, found := (*deps)[runCmd[0]]
	if !found {
		fmt.Printf("warn: command path not resolved %s\n", runCmd[0])
		run = runCmd
	} else {
		run = append([]string{cmdPath}, runCmd[1:]...)
	}

	for i := range run {
		run[i] = strings.Replace(run[i], "{WD}", targetDir, -1)
	}

	t := unsafeGet(template.ParseFS(templates, "templates/service.template"))

	data := struct {
		AppName    string
		ExecStart  string
		Env        []string
		WorkingDir string
		Uid        int
		Gid        int
	}{
		AppName:    name,
		ExecStart:  strings.Join(run, " "),
		Env:        appEnv,
		WorkingDir: targetDir,
		Uid:        uid,
		Gid:        gid,
	}

	f := unsafeGet(os.Create(systemdConf(name)))
	unsafe(t.Execute(f, data))
}

func proxyTemplate(host string, upstreamHost string, upstreamPort int, ws bool) {
	t := unsafeGet(template.ParseFS(templates, "templates/nginx.template"))

	data := struct {
		Host         string
		UpstreamPort int
		UpstreamHost string
		WS           bool
	}{
		Host:         host,
		UpstreamPort: upstreamPort,
		UpstreamHost: upstreamHost,
		WS:           ws,
	}

	logsDir := fmt.Sprintf("/var/log/%s", host)
	if _, err := os.Stat(logsDir); err != nil {
		unsafe(os.MkdirAll(logsDir, 0777))
	}

	certsDir := fmt.Sprintf("/etc/letsencrypt/live/%s", host)
	if _, err := os.Stat(certsDir); err != nil {
		fmt.Printf("warning: certs directory '%s' does not exist\n", certsDir)
	}

	f := unsafeGet(os.Create(nginxConf(host)))
	unsafe(t.Execute(f, data))
}

func removeOldVersions(host string, s *Service, r *Release) {
	dirPath := filepath.Join("/home", host)
	files := unsafeGet(ioutil.ReadDir(dirPath))
	prefix := fmt.Sprintf("%s-%s", s.Owner, s.Repo)

	for _, file := range files {
		name := file.Name()
		if !file.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}

		tag := name[len(prefix)+1:]
		if cmpVersion(r.Tag, tag) > 0 {
			dir := filepath.Join(dirPath, name)
			fmt.Printf("Removing directory: %s\n", dir)
			unsafe(os.RemoveAll(dir))
		}
	}
}

func install(
	s *Service,
	r *Release,
	conf *Conf,
	deps *map[string]string,
	appEnv []string,
	src string,
	name string,
	host string,
	upstreamHost string,
	upstreamPort int,
	ws bool) {
	uid, gid := ensureUser(name)

	targetDir := filepath.Join("/home", name, fmt.Sprintf("%s-%s-%s", s.Owner, s.Repo, r.Tag))
	if _, err := os.Stat(targetDir); err == nil {
		unsafe(os.RemoveAll(targetDir))
	}

	unsafe(os.MkdirAll(targetDir, 0777))

	fmt.Printf("Installing at the target directory: %s\n", targetDir)
	unsafe(cp.Copy(src, targetDir))

	if len(conf.InstallStep) > 0 {
		var cmd *exec.Cmd
		path, found := (*deps)[conf.InstallStep[0]]
		if !found {
			fmt.Printf("warn: path not resolved for %s\n", conf.InstallStep[0])
			cmd = command(conf.InstallStep...)
		} else {
			args := append([]string{path}, conf.InstallStep[1:]...)
			cmd = command(args...)
		}

		cmd.Dir = targetDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		if err != nil {
			log.Fatalf("install step '%s' failed, aborting\n", strings.Join(conf.InstallStep, " "))
		}
	}

	chown(targetDir, name)
	serviceTemplate(deps, name, conf.Run, appEnv, targetDir, uid, gid)
	if host != "" {
		proxyTemplate(host, upstreamHost, upstreamPort, ws)
	}
}

func workerName(w *Worker, name string) string {
	return fmt.Sprintf("%s-%s", w.Name, name)
}

func installWorker(
	s *Service,
	r *Release,
	deps *map[string]string,
	appEnv []string,
	name string,
	w *Worker) {
	uid, gid := ensureUser(name)
	targetDir := filepath.Join("/home", name, fmt.Sprintf("%s-%s-%s", s.Owner, s.Repo, r.Tag))
	serviceTemplate(deps, workerName(w, name), w.Run, appEnv, targetDir, uid, gid)
}

func parseArgs(args cli.Args) (Command, *Service) {
	if args.Len() > 2 || args.Len() == 0 {
		log.Fatal("one or two arguments expected")
	}

	c, ok := commands[strings.ToLower(args.First())]
	if !ok {
		log.Fatalf("invalid command: %s\n", args.First())
	}

	if c == Install {
		arg := args.Get(1)
		var tag string
		tagsplit := strings.Split(arg, ":")
		if len(tagsplit) > 2 {
			log.Fatalf("invalid argument: %s\n", arg)
		} else if len(tagsplit) == 2 {
			tag = tagsplit[1]
		} else {
			tag = "latest"
		}

		split := strings.Split(tagsplit[0], "/")
		if len(split) != 2 {
			log.Fatalf("invalid argument: %s\n", arg)
		}

		return c, &Service{
			Owner: split[0],
			Repo:  split[1],
			Tag:   tag,
		}
	}

	return c, nil
}

func storageDir() string {
	dir := "/var/local/instllr"
	if _, err := os.Stat(dir); err != nil {
		unsafe(os.MkdirAll(dir, 0777))
	}
	return dir
}

func storeVersion(host string, r *Release) {
	path := filepath.Join(storageDir(), host)
	if _, err := os.Stat(path); err == nil {
		unsafe(os.Remove(path))
	}

	unsafe(os.WriteFile(path, []byte(r.Tag), 0666))
}

func checkInstalledVersion(name string, r *Release) string {
	bs, err := os.ReadFile(filepath.Join(storageDir(), name))
	if err != nil {
		return ""
	}

	ver := string(bs)
	assertVersion(r.Tag, string(bs))
	return ver
}

func validateAssets(release *Release, assetName string) {
	if assetName == "" {
		if len(release.Assets) != 1 {
			log.Fatalf("Expected exactly one release asset, found %d", len(release.Assets))
		}
	} else {
		asset := release.GetAsset(assetName)
		if asset == nil {
			log.Fatal("asset not found")
		}
	}
}

func installCmd(s *Service, appEnv []string, name string, host string, upstreamHost string, upstreamPort int, assetName string, ws bool) {
	fmt.Printf("Installing %s\n", s.String())

	cfg := loadInstllrConfig()
	release := getGitHubRelease(cfg, s)
	validateAssets(release, assetName)

	currVer := checkInstalledVersion(name, release)
	if currVer == release.Tag {
		log.Fatalf("Version %s already installed\n", currVer)
	}

	dir := tmpDir()
	defer os.RemoveAll(dir)

	assetpath := fetchReleaseAsset(cfg, release.GetAsset(assetName), dir)
	fmt.Printf("Asset: %s\n", assetpath)

	untar(assetpath, dir)
	unsafe(os.Remove(assetpath))

	appCfg := loadConfig(dir)
	deps := resolveDeps(appCfg.Require)
	checkEnv(appCfg.Env, appEnv)

	cmd := exec.Command("systemctl", "stop", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	for _, w := range appCfg.Workers {
		cmd = exec.Command("systemctl", "stop", workerName(&w, name))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	install(s, release, appCfg, deps, appEnv, dir, name, host, upstreamHost, upstreamPort, ws)
	for _, w := range appCfg.Workers {
		installWorker(s, release, deps, appEnv, name, &w)
	}

	storeVersion(name, release)
	removeOldVersions(name, s, release)

	cmd = exec.Command("systemctl", "daemon-reload")
	cmd.Stderr = os.Stderr
	cmd.Run()

	cmd = exec.Command("systemctl", "enable", "--now", name)
	cmd.Stderr = os.Stderr
	cmd.Run()

	for _, w := range appCfg.Workers {
		cmd = exec.Command("systemctl", "enable", "--now", workerName(&w, name))
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	if host != "" {
		cmd = exec.Command("systemctl", "restart", "nginx")
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	fmt.Printf("\n%s has been installed successfully!\n", name)
}

func uninstallCmd(name string, host string) {
	fmt.Printf("Uninstalling %s\n", name)

	fmt.Printf("Stopping %s\n", name)
	cmd := exec.Command("systemctl", "stop", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	fmt.Printf("Disabling %s\n", name)
	cmd = exec.Command("systemctl", "disable", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	fmt.Printf("Removing config files: %s\n", name)
	os.Remove(systemdConf(name))
	os.Remove(filepath.Join(storageDir(), name))

	if host != "" {
		os.Remove(nginxConf(host))
		fmt.Println("Restarting nginx")
		cmd = exec.Command("systemctl", "restart", "nginx")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}
}

func main() {
	var host string
	var name string
	var upstreamHost string
	var upstreamPort int
	var appEnv cli.StringSlice
	var appEnvFile string
	var assetName string
	var ws bool

	app := &cli.App{
		Name:  "instllr",
		Usage: "install a service",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:        "app-env",
				Usage:       "Application env variables",
				Destination: &appEnv,
			},
			&cli.StringFlag{
				Name:        "app-env-file",
				Usage:       "Application env variables file",
				Destination: &appEnvFile,
			},
			&cli.StringFlag{
				Name:        "host",
				Usage:       "Hostname",
				Required:    false,
				Destination: &host,
			},
			&cli.StringFlag{
				Name:        "name",
				Usage:       "Name",
				Required:    false,
				Destination: &name,
			},
			&cli.StringFlag{
				Name:        "upstream-host",
				Usage:       "upstream host (default: 127.0.0.1)",
				Required:    false,
				Destination: &upstreamHost,
			},
			&cli.IntFlag{
				Name:        "port",
				Usage:       "upstream port",
				Required:    false,
				Destination: &upstreamPort,
			},
			&cli.StringFlag{
				Name:        "asset-name",
				Usage:       "Name of the asset (filename prefix)",
				Required:    false,
				Destination: &assetName,
			},
			&cli.BoolFlag{
				Name:        "ws",
				Usage:       "Enable WebSocket support in Nginx",
				Required:    false,
				Destination: &ws,
			},
		},
		Action: func(ctx *cli.Context) error {
			c, s := parseArgs(ctx.Args())
			serviceName := name
			if serviceName == "" {
				serviceName = host
			}

			if c == Install {
				if upstreamPort == 0 {
					log.Fatalf("invalid upstream port: %d\n", upstreamPort)
				}
				if host == "" && name == "" {
					log.Fatal("both host and name are empty")
				}
				if upstreamHost == "" {
					upstreamHost = "127.0.0.1"
				}

				env := appEnv.Value()
				if appEnvFile != "" {
					bs, err := os.ReadFile(appEnvFile)
					if err != nil {
						log.Fatalf("error reading app env file: %s\n", err)
					}

					lines := strings.Split(string(bs), "\n")
					for _, line := range lines {
						if line != "" {
							env = append(env, line)
						}
					}
				}

				installCmd(s, env, serviceName, host, upstreamHost, upstreamPort, assetName, ws)
			} else if c == Uninstall {
				uninstallCmd(serviceName, host)
			}

			return nil
		},
	}

	unsafe(app.Run(os.Args))
}
