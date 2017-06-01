package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/convox/rack/api/cmd/build/source"
	"github.com/convox/rack/api/structs"
	"github.com/convox/rack/manifest"
	"github.com/convox/rack/provider"
)

var (
	flagApp    string
	flagAuth   string
	flagCache  string
	flagID     string
	flagConfig string
	flagMethod string
	flagPush   string
	flagUrl    string

	currentBuild    *structs.Build
	currentLogs     string
	currentManifest string
	currentProvider provider.Provider

	event *structs.Event
)

func init() {
	currentProvider = provider.FromEnv()

	var buf bytes.Buffer

	currentProvider.Initialize(structs.ProviderOptions{
		LogOutput: &buf,
	})
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&flagApp, "app", "example", "app name")
	fs.StringVar(&flagAuth, "auth", "", "docker auth data (json)")
	fs.StringVar(&flagCache, "cache", "true", "use docker cache")
	fs.StringVar(&flagConfig, "config", "docker-compose.yml", "path to app config")
	fs.StringVar(&flagID, "id", "latest", "build id")
	fs.StringVar(&flagMethod, "method", "", "source method")
	fs.StringVar(&flagPush, "push", "", "push to registry")
	fs.StringVar(&flagUrl, "url", "", "source url")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fail(err)
	}

	if v := os.Getenv("BUILD_APP"); v != "" {
		flagApp = v
	}

	if v := os.Getenv("BUILD_AUTH"); v != "" {
		flagAuth = v
	}

	if v := os.Getenv("BUILD_CONFIG"); v != "" {
		flagConfig = v
	}

	if v := os.Getenv("BUILD_ID"); v != "" {
		flagID = v
	}

	if v := os.Getenv("BUILD_PUSH"); v != "" {
		flagPush = v
	}

	if v := os.Getenv("BUILD_URL"); v != "" {
		flagUrl = v
	}

	event = &structs.Event{
		Action: "build:create",
		Data: map[string]string{
			"app": flagApp,
			"id":  flagID,
		},
	}

	if err := execute(); err != nil {
		fail(err)
	}

	if err := export(); err != nil {
		fail(err)
	}

	if err := success(); err != nil {
		fail(err)
	}

	event.Status = "success"
	event.Data["release_id"] = currentBuild.Release
	if err := currentProvider.EventSend(event, nil); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
	}
	time.Sleep(1 * time.Second)
}

func execute() error {
	b, err := currentProvider.BuildGet(flagApp, flagID)
	if err != nil {
		return err
	}

	currentBuild = b

	if err := login(); err != nil {
		return err
	}

	dir, err := fetch()
	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	data, err := ioutil.ReadFile(filepath.Join(dir, flagConfig))
	if err != nil {
		return err
	}

	currentBuild.Manifest = string(data)

	if err := build(dir); err != nil {
		return err
	}

	return nil
}

func export() error {
	r, w := io.Pipe()

	errch := make(chan error)

	go createArtifact(w, errch)
	go uploadArtifact(r, errch)

	if err := <-errch; err != nil {
		return err
	}

	if err := <-errch; err != nil {
		return err
	}

	return nil
}

func createArtifact(w io.WriteCloser, errch chan error) {
	defer w.Close()

	b, err := currentProvider.BuildGet(flagApp, flagID)
	if err != nil {
		errch <- err
		return
	}

	currentBuild = b

	m, err := manifest.Load([]byte(currentBuild.Manifest))
	if err != nil {
		errch <- fmt.Errorf("manifest error: %s", err)
		return
	}

	if len(m.Services) < 1 {
		errch <- fmt.Errorf("no services found to export")
		return
	}

	bjson, err := json.MarshalIndent(currentBuild, "", "  ")
	if err != nil {
		errch <- err
		return
	}

	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		errch <- err
		return
	}

	defer os.Remove(tmp)

	pullch := make(chan error, len(m.Services))

	for service := range m.Services {
		go func() {
			image := fmt.Sprintf("%s/%s:latest", flagApp, service)
			file := filepath.Join(tmp, fmt.Sprintf("%s.%s.tar", service, currentBuild.Id))

			out, err := exec.Command("docker", "save", "-o", file, image).CombinedOutput()
			if err != nil {
				pullch <- fmt.Errorf("%s: %s\n", strings.TrimSpace(string(out)), err.Error())
			}

			pullch <- nil
		}()
	}

	for i := 0; i < len(m.Services); i++ {
		if err := <-pullch; err != nil {
			errch <- err
			return
		}
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	dataHeader := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "build.json",
		Mode:     0600,
		Size:     int64(len(bjson)),
	}

	if err := tw.WriteHeader(dataHeader); err != nil {
		errch <- err
		return
	}

	if _, err := tw.Write(bjson); err != nil {
		errch <- err
		return
	}

	for service := range m.Services {
		file := filepath.Join(tmp, fmt.Sprintf("%s.%s.tar", service, currentBuild.Id))

		stat, err := os.Stat(file)
		if err != nil {
			errch <- err
			return
		}

		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     fmt.Sprintf("%s.%s.tar", service, currentBuild.Id),
			Mode:     0600,
			Size:     stat.Size(),
		}

		if err := tw.WriteHeader(header); err != nil {
			errch <- err
			return
		}

		fd, err := os.Open(file)
		if err != nil {
			errch <- err
			return
		}

		if _, err := io.Copy(tw, fd); err != nil {
			errch <- err
			return
		}

		if err := os.Remove(file); err != nil {
			errch <- err
			return
		}
	}

	if err := tw.Close(); err != nil {
		errch <- err
		return
	}

	if err := gz.Close(); err != nil {
		errch <- err
		return
	}

	errch <- nil
}

func uploadArtifact(r io.Reader, errch chan error) {
	a, err := currentProvider.AppGet(flagApp)
	if err != nil {
		errch <- err
		return
	}

	ss, err := session.NewSession()
	if err != nil {
		errch <- err
		return
	}

	uploader := s3manager.NewUploader(ss)
	ui := &s3manager.UploadInput{
		Bucket: aws.String(a.Outputs["Settings"]),
		Key:    aws.String(fmt.Sprintf("exports/%s.tgz", flagID)),
		Body:   r,
	}

	_, err = uploader.Upload(ui)
	if err != nil {
		errch <- err
		return
	}

	errch <- nil
}

func fetch() (string, error) {
	var s source.Source

	switch flagMethod {
	case "git":
		s = &source.SourceGit{flagUrl}
	case "index":
		s = &source.SourceIndex{flagUrl}
	case "tgz":
		s = &source.SourceTgz{flagUrl}
	case "zip":
		s = &source.SourceZip{flagUrl}
	default:
		return "", fmt.Errorf("unknown method: %s", flagMethod)
	}

	var buf bytes.Buffer

	dir, err := s.Fetch(&buf)
	log(strings.TrimSpace(buf.String()))
	if err != nil {
		return "", err
	}

	return dir, nil
}

func login() error {
	var auth map[string]struct {
		Username string
		Password string
	}

	if err := json.Unmarshal([]byte(flagAuth), &auth); err != nil {
		return err
	}

	for host, entry := range auth {
		out, err := exec.Command("docker", "login", "-u", entry.Username, "-p", entry.Password, host).CombinedOutput()
		log(fmt.Sprintf("Authenticating %s: %s", host, strings.TrimSpace(string(out))))
		if err != nil {
			return err
		}
	}

	return nil
}

func build(dir string) error {
	dcy := filepath.Join(dir, flagConfig)

	if _, err := os.Stat(dcy); os.IsNotExist(err) {
		return fmt.Errorf("no such file: %s", flagConfig)
	}

	data, err := ioutil.ReadFile(dcy)
	if err != nil {
		return err
	}

	m, err := manifest.Load(data)
	if err != nil {
		return err
	}

	errs := m.Validate()
	if len(errs) > 0 {
		return errs[0]
	}

	s := make(chan string)

	go func() {
		for l := range s {
			log(l)
		}
	}()

	defer close(s)

	env, err := currentProvider.EnvironmentGet(flagApp)
	if err != nil {
		return err
	}

	a, err := currentProvider.AppGet(flagApp)
	if err != nil {
		return err
	}

	env["SECURE_ENVIRONMENT_URL"] = a.Parameters["Environment"]
	env["SECURE_ENVIRONMENT_TYPE"] = "envfile"
	env["SECURE_ENVIRONMENT_KEY"] = a.Parameters["Key"]
	env["AWS_REGION"] = os.Getenv("AWS_REGION")
	env["AWS_ACCESS_KEY_ID"] = os.Getenv("AWS_ACCESS")
	env["AWS_SECRET_ACCESS_KEY"] = os.Getenv("AWS_SECRET")

	err = m.Build(dir, flagApp, s, manifest.BuildOptions{
		Environment: env,
		Cache:       flagCache == "true",
		Verbose:     false,
	})
	if err != nil {
		return err
	}

	if err := m.Push(flagPush, flagApp, flagID, s); err != nil {
		return err
	}

	return nil
}

func success() error {
	_, err := currentProvider.BuildRelease(currentBuild)
	if err != nil {
		return err
	}

	url, err := currentProvider.ObjectStore(fmt.Sprintf("build/%s/logs", currentBuild.Id), bytes.NewReader([]byte(currentLogs)), structs.ObjectOptions{})
	if err != nil {
		return err
	}

	currentBuild.Ended = time.Now()
	currentBuild.Logs = url
	currentBuild.Status = "complete"

	if err := currentProvider.BuildSave(currentBuild); err != nil {
		return err
	}

	return nil
}

func fail(err error) {
	log(fmt.Sprintf("ERROR: %s", err))
	if e := currentProvider.EventSend(event, err); e != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", e)
	}

	url, _ := currentProvider.ObjectStore(fmt.Sprintf("build/%s/logs", currentBuild.Id), bytes.NewReader([]byte(currentLogs)), structs.ObjectOptions{})

	currentBuild.Ended = time.Now()
	currentBuild.Logs = url
	currentBuild.Reason = err.Error()
	currentBuild.Status = "failed"

	if err := currentProvider.BuildSave(currentBuild); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
	}

	os.Exit(1)
}

func log(line string) {
	currentLogs += fmt.Sprintf("%s\n", line)
	fmt.Println(line)
}
