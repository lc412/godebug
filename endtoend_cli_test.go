package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kylelemons/godebug/diff"
	"gopkg.in/yaml.v1"
)

// This file runs tests in the testdata directory, excluding those in testdata/single-file-tests

func TestCLISessions(t *testing.T) {
	godebug := compileGodebug(t)
	defer os.Remove(godebug)

	// Read the testdata directory
	fd, err := os.Open("testdata")
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	names, err := fd.Readdirnames(-1)
	if err != nil {
		t.Fatal("Readdirnames:", err)
	}
	tests := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, ".yaml") {
			tests = append(tests, name)
		}
	}

	// Run tests in parallel
	var wg sync.WaitGroup
	for _, test := range tests {
		for _, tt := range parseCases(t, filepath.Join("testdata", test)) {
			s := parseSessionFromBytes([]byte(tt.Transcript))
			for i := range tt.Invocations {
				wg.Add(1)
				go func(filename string, s *session, tt testCase, i int) {
					defer wg.Done()
					runTest(t, godebug, filename, tt, i, s)
				}(test, s, tt, i)
			}
		}
	}
	wg.Wait()
}

type testCase struct {
	Invocations []struct {
		Dir, Cmd string
	}
	Desc, Transcript string
	Creates          []string
	NonzeroExit      bool `yaml:"nonzero_exit"`
}

func parseCases(t *testing.T, filename string) []testCase {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	bb := bytes.Split(b, []byte("\n---\n"))
	testCases := make([]testCase, len(bb))
	for i := range bb {
		if err = yaml.Unmarshal(bb[i], &testCases[i]); err != nil {
			fmt.Println(string(bb[i]))
			t.Fatal(err)
		}
	}
	return testCases
}

func runTest(t *testing.T, godebug, filename string, tt testCase, i int, session *session) {
	var buf bytes.Buffer
	command, dir := tt.Invocations[i].Cmd, tt.Invocations[i].Dir
	cmd := exec.Command(godebug, strings.Split(command, " ")[1:]...)
	cmd.Dir = filepath.FromSlash("testdata/test-filesystem/" + dir)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Stdin = bytes.NewReader(session.input)
	setTestGopath(t, cmd)

	// Show multiple errors if they exist and format them nicely.
	var errs []string
	defer func() {
		if errs != nil {
			t.Errorf("File: %s\nDescription: %s\nWorking dir: %s\nCommand: %s\nFailures:\n\t%v",
				filename, tt.Desc, dir, command, strings.Join(errs, "\n\t"))
		}
	}()

	cmd.Env = append(cmd.Env, logFileEnvVar+"=true")
	err := cmd.Run()
	// Because we set `logFileEnvVar` above, godebug will print the
	// files it creates to stdout. Parse those lines and then pretend
	// they were not printed.
	createdFiles, output := recordCreatedFiles(buf.Bytes())

	switch err.(type) {
	case nil:
		if tt.NonzeroExit {
			errs = append(errs, "got exit code == 0, wanted a nonzero exit code.")
			return
		}
	case *exec.ExitError:
		if !tt.NonzeroExit {
			errs = append(errs, fmt.Sprintf("%q failed to run: %v\n%s", command, err, output))
			return
		}
	default:
		errs = append(errs, fmt.Sprintf("%q failed to run: %v\n%s", command, err, output))
		return
	}

	// Check that we created the files we expected and did not create
	// any files we did not expect.
	errs = append(errs, checkCreatedFiles(t, tt.Creates, createdFiles)...)

	got := interleaveCommands(session.input, output)
	if equivalent(got, session.fullSession) {
		return
	}
	errs = append(errs, fmt.Sprintf("golden transcript did not match actual transcript. Diff:\n\n%v", diff.Diff(string(session.fullSession), string(got))))
}

func checkCreatedFiles(t *testing.T, g, w []string) (errs []string) {
	got, want := listToMap(g), listToMap(w)
	for f := range got {
		if !want[f] {
			errs = append(errs, "created a file we did not want: "+f)
		}
	}
	for f := range want {
		if !got[f] {
			errs = append(errs, "did not create a file we wanted: "+f)
		}
	}
	return errs
}

func recordCreatedFiles(b []byte) (files []string, rest []byte) {
	bb := bytes.Split(b, newline)

	for i := range bb {
		if bytes.HasPrefix(bb[i], []byte(logFilePrefix)) {
			files = append(files, string(bb[i][len(logFilePrefix):]))
		} else {
			rest = append(rest, bb[i]...)
			if i+1 < len(bb) {
				rest = append(rest, newline...)
			}
		}
	}
	return files, rest
}

func listToMap(list []string) map[string]bool {
	m := make(map[string]bool)
	for _, s := range list {
		m[s] = true
	}
	return m
}

// equivalent does a linewise comparison of a and b.
// Each line must be exactly equal or the want line must end in "//substr"
// and be a substring of the got line.
// Otherwise equivalent returns false.
func equivalent(got, want []byte) bool {
	var (
		gotLines  = bytes.Split(got, newline)
		wantLines = bytes.Split(want, newline)
		substr    = []byte("//substr")
		gg, ww    []byte
	)

	if len(gotLines) != len(wantLines) {
		return false
	}

	for i := range gotLines {
		gg, ww = gotLines[i], wantLines[i]
		if !(bytes.Equal(gg, ww) || bytes.HasSuffix(ww, substr) && bytes.Contains(gg, ww[:len(ww)-len(substr)])) {
			return false
		}
	}
	return true
}

func setTestGopath(t *testing.T, cmd *exec.Cmd) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	setGopath(cmd, filepath.Join(cwd, "testdata", "test-filesystem", "gopath"))
}
