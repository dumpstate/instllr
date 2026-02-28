package main

import (
	"archive/tar"
	"bytes"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xi2/xz"
)

func tmpDir(base string) string {
	return unsafeGet(ioutil.TempDir(base, ""))
}

func untar(path string, target string) {
	reader := unsafeGet(os.Open(path))
	defer reader.Close()

	r := unsafeGet(xz.NewReader(reader, 0))
	tarReader := tar.NewReader(r)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			unsafe(os.MkdirAll(filepath.Join(target, header.Name), 0777))
		case tar.TypeReg, tar.TypeRegA:
			fp := filepath.Join(target, header.Name)
			unsafe(os.MkdirAll(filepath.Dir(fp), 0777))

			w := unsafeGet(os.Create(fp))

			unsafeGet(io.Copy(w, tarReader))
			w.Close()

			unsafe(os.Chmod(fp, header.FileInfo().Mode()))
		}
	}
}

func id(uname string, flag string) (int, error) {
	cmd := exec.Command("id", flag, uname)

	var out bytes.Buffer
	cmd.Stdout = &out

	err := cmd.Run()
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(strings.TrimSpace(out.String()))
}

func chown(root string, uname string) {
	uid, gid := unsafeGet(id(uname, "-u")), unsafeGet(id(uname, "-g"))
	unsafe(filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		unsafe(os.Chown(path, uid, gid))

		return nil
	}))
}
