package spawner

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	l "github.com/Shanduur/spawner/logger"
)

type Component struct {
	Entrypoint []string    `yaml:"entrypoint"`
	Cmd        []string    `yaml:"cmd"`
	Depends    string      `yaml:"depends"`
	WorkDir    string      `yaml:"workdir"`
	After      []Component `yaml:"after"`
	Before     []Component `yaml:"before"`
	Tee        Tee         `yaml:"tee"`
	ExecCmd    *exec.Cmd

	populated bool
	prefix    string

	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

func (cmd Component) String() string {
	var cmdArray []string
	cmdArray = append(cmdArray, cmd.Entrypoint...)
	cmdArray = append(cmdArray, cmd.Cmd...)

	return cmdArray[0]
}

func (cmd *Component) AddPrefix(prefix string) error {
	for i := 0; i < len(cmd.Before); i++ {
		if err := cmd.Before[i].AddPrefix(prefix); err != nil {
			return err
		}
	}

	for i := 0; i < len(cmd.After); i++ {
		if err := cmd.After[i].AddPrefix(prefix); err != nil {
			return err
		}
	}

	cmd.WorkDir = path.Join(prefix, cmd.WorkDir)
	cmd.prefix = prefix

	return nil
}

func (cmd *Component) Kill() {
	for i := 0; i < len(cmd.Before); i++ {
		cmd.Before[i].Kill()
	}

	if cmd.ExecCmd.Process != nil {
		if err := cmd.ExecCmd.Process.Kill(); err != nil {
			l.Log().Warn(err)
		}
	}

	for i := 0; i < len(cmd.After); i++ {
		cmd.After[i].Kill()
	}
}

func (cmd *Component) Populate() error {
	for i := 0; i < len(cmd.Before); i++ {
		if err := cmd.Before[i].Populate(); err != nil {
			return err
		}
	}

	for i := 0; i < len(cmd.After); i++ {
		if err := cmd.After[i].Populate(); err != nil {
			return err
		}
	}

	cmd.populated = true

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	wd, err := filepath.Abs(cmd.WorkDir)
	if err != nil {
		return err
	}
	if len(wd) > len(cmd.WorkDir) {
		cmd.WorkDir = wd
	}
	// l.Log().Info(cmd.WorkDir)

	if err = os.MkdirAll(cmd.WorkDir, 0777); err != nil {
		return err
	}

	if err = cmd.Tee.Open(path.Join(
		cmd.prefix,
		strings.ReplaceAll(cmd.String(), " ", "_"),
	)); err != nil {
		return err
	}

	var componentArray []string
	componentArray = append(componentArray, cmd.Entrypoint...)
	componentArray = append(componentArray, cmd.Cmd...)

	if len(componentArray) <= 0 {
		return fmt.Errorf("neither entrypoint nor component provided")
	}

	componentArray, err = cmd.ArrayExpand(componentArray)
	if err != nil {
		return fmt.Errorf("unable to expand array: %w", err)
	}

	name := componentArray[0]
	var args []string
	if len(componentArray) > 1 {
		args = append(args, componentArray[1:]...)
	}

	cmd.ExecCmd = exec.Command(name, args...)
	cmd.Stdout, err = cmd.ExecCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("unable to create stdout pipe for %s", cmd.String())
	}

	cmd.Stderr, err = cmd.ExecCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("unable to create stderr pipe for %s", cmd.String())
	}

	cmd.ExecCmd.Dir = cmd.WorkDir

	return nil
}

func NewComponent(entrypoint string) *Component {
	return &Component{
		Entrypoint: []string{entrypoint},
	}
}

type ErrExecutionError struct {
	argEntrypoint string
	reason        error
}

func NewErrExecutionError(argEntrypoint string, err error) ErrExecutionError {
	return ErrExecutionError{
		argEntrypoint: argEntrypoint,
		reason:        err,
	}
}

func (err ErrExecutionError) Error() string {
	return fmt.Sprintf("execution of %s failed, reason: %s", err.argEntrypoint, err.reason)
}

func (cmd Component) ArrayExpand(array []string) ([]string, error) {
	for i := 0; i < len(array); i++ {
		var buf bytes.Buffer
		tpl, err := template.New(cmd.String()).Parse(array[i])
		if err != nil {
			return []string{}, err
		}

		if err := tpl.Execute(&buf, cmd); err == nil {
			array[i] = buf.String()
		}
	}

	return array, nil
}

func (cmd *Component) Exec(ctx context.Context) error {
	var err error

	if !cmd.populated {
		err := cmd.Populate()
		if err != nil {
			return fmt.Errorf("error during populating component %s: %w", cmd.String(), err)
		}
	}

	for _, beforeCmd := range cmd.Before {
		err := beforeCmd.Exec(ctx)
		if err != nil {
			return err
		}
	}

	go func() {
		var w io.Writer
		if cmd.Tee.Combined || cmd.Tee.Stdout {
			w = io.MultiWriter(cmd.Tee.StdoutFile)
			_, err := io.Copy(w, cmd.Stdout)
			if err != nil {
				l.Log().Printf("stdout error %s: %s", cmd.String(), err.Error())
			}
		}
	}()

	go func() {
		var w io.Writer
		if cmd.Tee.Combined || cmd.Tee.Stderr {
			w = io.MultiWriter(cmd.Tee.StderrFile)
			_, err := io.Copy(w, cmd.Stderr)
			if err != nil {
				l.Log().Printf("stderr error %s: %s", cmd.String(), err.Error())
			}
		}
	}()

	err = cmd.ExecCmd.Start()
	if err != nil {
		return NewErrExecutionError(cmd.String(), err)
	}

	err = cmd.ExecCmd.Wait()
	if err != nil {
		return NewErrExecutionError(cmd.String(), err)
	}

	for _, afterCmd := range cmd.After {
		err := afterCmd.Exec(ctx)
		if err != nil {
			return err
		}
	}

	return err
}

type Tee struct {
	Stdout     bool `yaml:"stdout"`
	Stderr     bool `yaml:"stderr"`
	Combined   bool `yaml:"combined"`
	StderrFile *os.File
	StdoutFile *os.File
}

func (t *Tee) Open(name string) error {
	if t.Combined {
		f, err := os.OpenFile(name+".log", os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		t.StdoutFile = f
		t.StderrFile = f
		return nil
	}

	if t.Stdout {
		f, err := os.OpenFile(name+".log", os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		t.StdoutFile = f
	}

	if t.Stderr {
		f, err := os.OpenFile(name+".err", os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		t.StderrFile = f
	}

	return nil
}

func (t *Tee) Close() {
	if t.Stdout {
		t.StdoutFile.Close()
	}
	if t.Stderr {
		t.StderrFile.Close()
	}
	if t.Combined {
		t.StdoutFile.Close()
	}
}
