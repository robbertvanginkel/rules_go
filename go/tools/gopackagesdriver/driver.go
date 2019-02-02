//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/rules_go/go/tools/gopackagesdriver/build_event_stream"
	"github.com/golang/protobuf/proto"
	"golang.org/x/tools/go/packages"
)

var programName = filepath.Base(os.Args[0])

const usageFmt = `Usage: %s patterns...

Bazel gopackagesdriver gathers metadata about packages in a Bazel workspace
and prints that information on stdout in json format. gopackagesdriver is
intended to be invoked by golang.org/x/tools/go/packages when the
GOPACKAGESDRIVER environment variable is set.

`

// driverRequest copied from package
type driverRequest struct {
	Command    string            `json "command"`
	Mode       packages.LoadMode `json:"mode"`
	Env        []string          `json:"env"`
	BuildFlags []string          `json:"build_flags"`
	Tests      bool              `json:"tests"`
	Overlay    map[string][]byte `json:"overlay"`
}

// driverResponse copied from package
type driverResponse struct {
	Sizes    *types.StdSizes
	Roots    []string `json:",omitempty"`
	Packages []*packages.Package
}

func main() {
	log.SetPrefix(programName + ": ")
	log.SetFlags(0)

	var record driverRequest
	err := json.NewDecoder(os.Stdin).Decode(&record)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := list(record, os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		log.Fatal(err)
	}
}

func list(request driverRequest, args []string) (driverResponse, error) {
	binDir, workspaceDir, execDir, err := getBazelDirs()
	if err == noWorkspaceError {
		return listFallback(request, args)
	} else if err != nil {
		return driverResponse{}, err
	}

	filesToQuery, patterns, stdlibPatterns := parsePatterns(os.Args[1:])

	targets, err := queryTargets(workspaceDir, request.BuildFlags, filesToQuery, patterns)
	if err != nil {
		return driverResponse{}, err
	}

	files, err := buildPkgFiles(request.Mode, request.BuildFlags, targets, len(stdlibPatterns) > 0)
	if err != nil {
		return driverResponse{}, err
	}
	for i := range files {
		files[i] = filepath.Join(binDir, files[i])
	}

	pkgs, err := loadPkgFiles(files)
	if err != nil {
		return driverResponse{}, err
	}
	for _, pkg := range pkgs {
		absPkgPaths(pkg, workspaceDir, execDir)
		pkgs = append(pkgs, pkg)
	}

	resp := driverResponse{
		// TODO: figure out what size we actually need and where to get them
		Sizes:    &types.StdSizes{8, 8},
		Roots:    append(targets, stdlibPatterns...),
		Packages: pkgs,
	}
	return resp, nil
}

func getBazelDirs() (binDir, workspaceDir, execDir string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	for {
		_, err := os.Stat(filepath.Join(dir, "WORKSPACE"))
		if err == nil {
			workspaceDir = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", "", noWorkspaceError
		}
		dir = parent
	}

	out, err := exec.Command("bazel", "info").Output()
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		var key, value string
		sep := ": "
		if i := strings.Index(line, sep); i < 0 {
			continue
		} else {
			key = line[:i]
			value = line[i+len(sep):]
		}
		switch key {
		case "bazel-bin":
			binDir = value
		case "execution_root":
			execDir = value
		}
	}
	return binDir, workspaceDir, execDir, nil
}

func queryTargets(workspaceDir string, buildFlags, filesToQuery, patterns []string) ([]string, error) {
	// to go from files -> targets, use same_pkg_direct_rdeps
	// https://docs.bazel.build/versions/master/query-how-to.html#what-rule-target-s-contain-file-path-to-file-bar-java-as-a-sourc
	var filesQuery = make([]string, 0)
	for _, s := range filesToQuery {
		filesQuery = append(filesQuery, "same_pkg_direct_rdeps("+strings.TrimPrefix(s, workspaceDir+"/")+")")
	}

	args := []string{"query"}
	args = append(args, buildFlags...)
	args = append(args, "--")
	// TODO: should properly combine query params, this isn't necessarily valid. Works now because we only ever get either
	// patterns or a file.
	args = append(args, patterns...)
	args = append(args, filesQuery...)
	cmd := exec.Command("bazel", args...)
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var targets []string
	data := buf.Bytes()
	i := bytes.IndexByte(data, '\n')
	for i >= 0 {
		target := string(data[:i])
		data = data[i+1:]
		i = bytes.IndexByte(data, '\n')
		if len(target) > 0 && target[len(target)-1] == '\r' {
			target = target[:len(target)-1]
		}
		targets = append(targets, target)
	}
	return targets, nil
}

const (
	aspectFileName  = "@io_bazel_rules_go//go/tools/gopackagesdriver:aspect.bzl"
	outputGroupName = "gopackagesdriver"
	stdlibTarget    = "@io_bazel_rules_go//:stdlib"

	// TODO: find a better way to fetch the directory where the stdlib was build
	// than using this file. Keep name in sync with what's in the aspect.
	stdlibMarkerFilename = "stdlib_magical_value.txt"
)

func buildPkgFiles(mode packages.LoadMode, buildFlags, targets []string, buildStdlib bool) ([]string, error) {
	logFile, err := ioutil.TempFile("", "gopackagesdriver")
	if err != nil {
		return nil, err
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	var b strings.Builder
	b.WriteString(aspectFileName)
	b.WriteString("%gopackagesdriver_")
	// TODO: figre out what aspect we need based on the mode
	if true { // needDeps {
		b.WriteString("deps_")
	}
	if true { // needExport {
		b.WriteString("export_")
	}
	if true { // needTest {
		b.WriteString("test_")
	}
	b.WriteString("aspect")
	aspectName := b.String()
	args := []string{
		"build",
		// "--nocheck_visibility", // TODO: figure out if needed, saw sporadic errors around //go/tools/builders:nogo_srcs
		"--verbose_failures",
		"-s",
		"--spawn_strategy=standalone",
		"--aspects=" + aspectName,
		"--output_groups=" + outputGroupName,
		"--build_event_binary_file=" + logPath,
	}
	args = append(args, buildFlags...)
	args = append(args, "--")
	args = append(args, targets...)
	if buildStdlib {
		args = append(args, stdlibTarget)
	}
	cmd := exec.Command("bazel", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	logData, err := ioutil.ReadFile(logPath)
	if err != nil {
		return nil, err
	}
	var event build_event_stream.BuildEvent
	setToFiles := make(map[string][]string)
	setToSets := make(map[string][]string)
	var rootSets []string
	for len(logData) > 0 {
		size, n := proto.DecodeVarint(logData)
		if n == 0 {
			return nil, err
			break
		}
		logData = logData[n:]
		if err := proto.Unmarshal(logData[:size], &event); err != nil {
			return nil, err
		}
		logData = logData[size:]

		if id := event.GetId().GetTargetCompleted(); id != nil {
			if id.GetAspect() != aspectName {
				continue
			}
			completed := event.GetCompleted()
			if !completed.GetSuccess() {
				return nil, fmt.Errorf("%s did not build successfully", id.GetLabel())
			}
			for _, g := range completed.GetOutputGroup() {
				if g.GetName() != outputGroupName {
					continue
				}
				for _, s := range g.GetFileSets() {
					if setId := s.GetId(); setId != "" {
						rootSets = append(rootSets, setId)
					}
				}
			}
			continue
		}

		if id := event.GetId().GetNamedSet(); id != nil {
			files := event.GetNamedSetOfFiles().GetFiles()
			fileNames := make([]string, len(files))
			for i, f := range files {
				fileNames[i] = f.GetName()
			}
			setToFiles[id.GetId()] = fileNames
			sets := event.GetNamedSetOfFiles().GetFileSets()
			setIds := make([]string, len(sets))
			for i, s := range sets {
				setIds[i] = s.GetId()
			}
			setToSets[id.GetId()] = setIds
			continue
		}
	}

	files := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(setId string) {
		if visited[setId] {
			return
		}
		visited[setId] = true
		for _, f := range setToFiles[setId] {
			files[f] = true
		}
		for _, s := range setToSets[setId] {
			visit(s)
		}
	}
	for _, s := range rootSets {
		visit(s)
	}
	sortedFiles := make([]string, 0, len(files))
	for f := range files {
		sortedFiles = append(sortedFiles, f)
	}
	sort.Strings(sortedFiles)
	return sortedFiles, nil
}

func loadPkgFiles(paths []string) ([]*packages.Package, error) {
	var pkgs []*packages.Package
	for _, path := range paths {
		// TODO: figure out how to not rely on this magic value
		if strings.HasSuffix(path, stdlibMarkerFilename) {
			path = strings.TrimSuffix(path, stdlibMarkerFilename)
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			dirPkgs, err := loadPkgDir(path)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, dirPkgs...)
		} else {
			pkg, err := loadPkgFile(path)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}

func loadPkgFile(pkgFilePath string) (*packages.Package, error) {
	pkgData, err := ioutil.ReadFile(pkgFilePath)
	if err != nil {
		return nil, err
	}
	pkg := &packages.Package{}
	if err := json.Unmarshal(pkgData, pkg); err != nil {
		return nil, err
	}
	return pkg, nil
}

func loadPkgDir(pkgDirPath string) ([]*packages.Package, error) {
	var pkgs []*packages.Package
	err := filepath.Walk(pkgDirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// TODO: the path that contains all the json files for the stdlib also contains the go cache
		// that contains files that the stdlib json files refer to. These are not json files, so
		// ignore them.
		if strings.HasSuffix(path, stdlibMarkerFilename) || strings.Contains(path, ".gocache") {
			return nil
		}
		pkg, err := loadPkgFile(path)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, pkg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pkgs, nil
}

func absPkgPaths(pkg *packages.Package, workspaceDir, execDir string) {
	// TODO: depending on packages from the repo, external repos or from the stdlib
	// they end up here with different absolute or relative paths. This mapping works
	// for now but should figure out something solid.
	absPathIfNecessary := func(path string) string {
		if strings.HasPrefix(path, "external/") {
			return filepath.Join(execDir, path)
		} else if !strings.HasPrefix(path, "/") {
			return filepath.Join(workspaceDir, path)
		}
		return path
	}
	for i := range pkg.GoFiles {
		pkg.GoFiles[i] = absPathIfNecessary(pkg.GoFiles[i])
	}
	for i := range pkg.CompiledGoFiles {
		pkg.CompiledGoFiles[i] = absPathIfNecessary(pkg.CompiledGoFiles[i])
	}
	for i := range pkg.OtherFiles {
		pkg.OtherFiles[i] = absPathIfNecessary(pkg.OtherFiles[i])
	}
	if pkg.ExportFile != "" {
		pkg.ExportFile = absPathIfNecessary(pkg.ExportFile)
	}
}

var noWorkspaceError = errors.New("working directory is outside any Bazel workspace")

func listFallback(request driverRequest, patterns []string) (driverResponse, error) {
	cfg := &packages.Config{
		Mode:       request.Mode,
		Env:        append(os.Environ(), "GOPACKAGESDRIVER=off"),
		BuildFlags: request.BuildFlags,
		Tests:      request.Tests,
		Overlay:    request.Overlay,
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		return driverResponse{}, nil
	}

	// TODO: I think we need sizes added here
	resp := driverResponse{}
	seen := make(map[*packages.Package]bool)
	var visit func(*packages.Package)
	visit = func(pkg *packages.Package) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		resp.Packages = append(resp.Packages, pkg)
		for _, ipkg := range pkg.Imports {
			visit(ipkg)
		}
	}
	for _, pkg := range roots {
		resp.Roots = append(resp.Roots, pkg.ID)
		visit(pkg)
	}
	return resp, nil
}

// multiFlag allows repeated string flags to be collected into a slice
type multiFlag []string

func (m *multiFlag) String() string {
	if len(*m) == 0 {
		return ""
	}
	return fmt.Sprint(*m)
}

func (m *multiFlag) Set(v string) error {
	(*m) = append(*m, v)
	return nil
}

func isStdlibPattern(pattern string) bool {
	return strings.HasPrefix(pattern, "@io_bazel_rules_go//:stdlib%")
}

// mostly copied from goListDriver in packages, modified to detect bazel stdlib
func parsePatterns(patterns []string) ([]string, []string, []string) {
	// Determine files requested in contains patterns
	var containFiles []string
	restPatterns := make([]string, 0, len(patterns))
	stdlibPatterns := make([]string, 0, len(patterns))
	// Extract file= and other [querytype]= patterns. Report an error if querytype
	// doesn't exist.
extractQueries:
	for _, pattern := range patterns {
		eqidx := strings.Index(pattern, "=")
		if eqidx < 0 {
			if isStdlibPattern(pattern) {
				stdlibPatterns = append(stdlibPatterns, pattern)
			} else {
				restPatterns = append(restPatterns, pattern)
			}
		} else {
			query, value := pattern[:eqidx], pattern[eqidx+len("="):]
			switch query {
			case "file":
				containFiles = append(containFiles, value)
			case "pattern":
				restPatterns = append(restPatterns, value)
			case "iamashamedtousethedisabledqueryname": // old value, ignore
				continue
			case "": // not a reserved query
				if isStdlibPattern(pattern) {
					stdlibPatterns = append(stdlibPatterns, pattern)
				} else {
					restPatterns = append(restPatterns, pattern)
				}
			default:
				for _, rune := range query {
					if rune < 'a' || rune > 'z' { // not a reserved query
						if isStdlibPattern(pattern) {
							stdlibPatterns = append(stdlibPatterns, pattern)
						} else {
							restPatterns = append(restPatterns, pattern)
						}
						continue extractQueries
					}
				}
				// Reject all other patterns containing "="
				panic(fmt.Errorf("invalid query type %q in query pattern %q", query, pattern))
			}
		}
	}
	return containFiles, restPatterns, stdlibPatterns
}
