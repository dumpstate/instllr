package main

import (
	"embed"
	"fmt"
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

func serviceTemplate(deps *map[string]string, host string, conf *Conf, appEnv []string, targetDir string, uid int, gid int) {
	var run []string
	cmdPath, found := (*deps)[conf.Run[0]]
	if !found {
		fmt.Printf("warn: command path not resolved %s\n", conf.Run[0])
		run = conf.Run
	} else {
		run = append([]string{cmdPath}, conf.Run[1:]...)
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
		AppName:    host,
		ExecStart:  strings.Join(run, " "),
		Env:        appEnv,
		WorkingDir: targetDir,
		Uid:        uid,
		Gid:        gid,
	}

	f := unsafeGet(os.Create(fmt.Sprintf("/etc/systemd/system/%s.service", host)))
	unsafe(t.Execute(f, data))
}

func proxyTemplate(host string, port int) {
	t := unsafeGet(template.ParseFS(templates, "templates/nginx.template"))

	data := struct {
		Host string
		Port int
	}{
		Host: host,
		Port: port,
	}

	logsDir := fmt.Sprintf("/var/log/%s", host)
	if _, err := os.Stat(logsDir); err != nil {
		unsafe(os.MkdirAll(logsDir, 0777))
	}

	certsDir := fmt.Sprintf("/etc/letsencrypt/live/%s", host)
	if _, err := os.Stat(certsDir); err != nil {
		fmt.Printf("warning: certs directory '%s' does not exist\n", certsDir)
	}

	f := unsafeGet(os.Create(fmt.Sprintf("/etc/nginx/sites-enabled/%s.conf", host)))
	unsafe(t.Execute(f, data))
}

func install(
	conf *Conf,
	deps *map[string]string,
	appEnv []string,
	src string,
	owner string,
	repo string,
	tag string,
	host string,
	port int) {
	uid, gid := ensureUser(host)

	targetDir := filepath.Join("/home", host, fmt.Sprintf("%s-%s-%s", owner, repo, tag))
	if _, err := os.Stat(targetDir); err == nil {
		log.Fatalf("directory %s already exists, aborting", targetDir)
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

	chown(targetDir, host)
	serviceTemplate(deps, host, conf, appEnv, targetDir, uid, gid)
	proxyTemplate(host, port)
}

func parseArg(arg string) (string, string, string) {
	if arg == "" {
		log.Fatal("at least one argument expected")
	}

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

	return split[0], split[1], tag
}

func main() {
	var host string
	var port int
	var appEnv cli.StringSlice

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
				Name:        "host",
				Usage:       "Hostname",
				Required:    true,
				Destination: &host,
			},
			&cli.IntFlag{
				Name:        "port",
				Usage:       "local application port",
				Required:    true,
				Destination: &port,
			},
		},
		Action: func(ctx *cli.Context) error {
			owner, repo, tag := parseArg(ctx.Args().First())
			fmt.Printf("Installing %s/%s:%s\n", owner, repo, tag)

			release := getGitHubRelease(owner, repo, tag)
			if len(release.Assets) != 1 {
				log.Fatalf("Expected exactly one release asset, found %d", len(release.Assets))
			}

			dir := tmpDir()
			defer os.RemoveAll(dir)

			assetpath := fetchReleaseAsset(&release.Assets[0], dir)
			fmt.Printf("Asset: %s\n", assetpath)

			untar(assetpath, dir)
			unsafe(os.Remove(assetpath))

			conf := loadConfig(dir)
			deps := resolveDeps(conf.Require)
			checkEnv(conf.Env, appEnv.Value())

			install(conf, deps, appEnv.Value(), dir, owner, repo, release.Tag, host, port)

			fmt.Printf("\n%s has been installed successfully!\n\nNext:\n", host)
			fmt.Printf("1. Enable and start the service: systemctl enable --now %s\n", host)
			fmt.Println("2. Request certificate from certbot")
			fmt.Printf("3. Re-start nginx: systemctl restart nginx\n")

			return nil
		},
	}

	unsafe(app.Run(os.Args))
}
