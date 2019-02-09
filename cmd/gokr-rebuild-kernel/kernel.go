package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM debian:stretch

RUN apt-get update && apt-get install -y crossbuild-essential-arm64 bc libssl-dev bison flex

COPY gokr-build-kernel /usr/bin/gokr-build-kernel
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src

USER builduser
WORKDIR /usr/src
ENTRYPOINT /usr/bin/gokr-build-kernel
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

var patchFiles = []string{
	"0001-Revert-add-index-to-the-ethernet-alias.patch",
	// serial
	"0101-expose-UART0-ttyAMA0-on-GPIO-14-15-disable-UART1-tty.patch",
	"0102-expose-UART0-ttyAMA0-on-GPIO-14-15-disable-UART1-tty.patch",
}

func copyFile(dest, src string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
}

var gopath = mustGetGopath()

func mustGetGopath() string {
	gopathb, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		log.Panic(err)
	}
	return strings.TrimSpace(string(gopathb))
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	path := filepath.Join(gopath, "src", "github.com", "gokrazy", "kernel", filename)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("could not find file %q (looked in . and %s)", filename, path)
}

func main() {
	// We explicitly use /tmp, because Docker only allows volume mounts under
	// certain paths on certain platforms, see
	// e.g. https://docs.docker.com/docker-for-mac/osxfs/#namespaces for macOS.
	tmp, err := ioutil.TempDir("/tmp", "gokr-rebuild-kernel")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command("go", "build", "github.com/gokrazy/kernel/cmd/gokr-build-kernel")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOOS=linux")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%v: %v", cmd.Args, err)
	}

	buildPath := filepath.Join(tmp, "gokr-build-kernel")

	var patchPaths []string
	for _, filename := range patchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	kernelPath, err := find("vmlinuz")
	if err != nil {
		log.Fatal(err)
	}
	dtbPath, err := find("bcm2710-rpi-3-b.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbPlusPath, err := find("bcm2710-rpi-3-b-plus.dtb")
	if err != nil {
		log.Fatal(err)
	}

	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err := copyFile(filepath.Join(tmp, filepath.Base(path)), path); err != nil {
			log.Fatal(err)
		}
	}

	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	dockerFile, err := os.Create(filepath.Join(tmp, "Dockerfile"))
	if err != nil {
		log.Fatal(err)
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid       string
		Gid       string
		BuildPath string
		Patches   []string
	}{
		Uid:       u.Uid,
		Gid:       u.Gid,
		BuildPath: buildPath,
		Patches:   patchFiles,
	}); err != nil {
		log.Fatal(err)
	}

	if err := dockerFile.Close(); err != nil {
		log.Fatal(err)
	}

	log.Printf("building docker container for kernel compilation")

	dockerBuild := exec.Command("docker",
		"build",
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Dir = tmp
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	if err := dockerBuild.Run(); err != nil {
		log.Fatalf("docker build: %v (cmd: %v)", err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	dockerRun := exec.Command("docker",
		"run",
		"--rm",
		"--volume", tmp+":/tmp/buildresult:Z",
		"gokr-rebuild-kernel")
	dockerRun.Dir = tmp
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	if err := dockerRun.Run(); err != nil {
		log.Fatalf("docker run: %v (cmd: %v)", err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, filepath.Join(tmp, "vmlinuz")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPath, filepath.Join(tmp, "bcm2710-rpi-3-b.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPlusPath, filepath.Join(tmp, "bcm2710-rpi-3-b-plus.dtb")); err != nil {
		log.Fatal(err)
	}
}
