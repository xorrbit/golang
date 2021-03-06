// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codeProject      = "go"
	codePyScript     = "misc/dashboard/googlecode_upload.py"
	hgUrl            = "https://code.google.com/p/go/"
	mkdirPerm        = 0750
	waitInterval     = 30 * time.Second // time to wait before checking for new revs
	pkgBuildInterval = 24 * time.Hour   // rebuild packages every 24 hours
)

// These variables are copied from the gobuilder's environment
// to the envv of its subprocesses.
var extraEnv = []string{
	"CC",
	"GOARM",
	"GOHOSTARCH",
	"GOHOSTOS",
	"PATH",
	"TMPDIR",
}

type Builder struct {
	name         string
	goos, goarch string
	key          string
}

var (
	buildroot      = flag.String("buildroot", defaultBuildRoot(), "Directory under which to build")
	dashboard      = flag.String("dashboard", "build.golang.org", "Go Dashboard Host")
	buildRelease   = flag.Bool("release", false, "Build and upload binary release archives")
	buildRevision  = flag.String("rev", "", "Build specified revision and exit")
	buildCmd       = flag.String("cmd", filepath.Join(".", allCmd), "Build command (specify relative to go/src/)")
	failAll        = flag.Bool("fail", false, "fail all builds")
	parallel       = flag.Bool("parallel", false, "Build multiple targets in parallel")
	buildTimeout   = flag.Duration("buildTimeout", 60*time.Minute, "Maximum time to wait for builds and tests")
	cmdTimeout     = flag.Duration("cmdTimeout", 5*time.Minute, "Maximum time to wait for an external command")
	commitInterval = flag.Duration("commitInterval", 1*time.Minute, "Time to wait between polling for new commits")
	verbose        = flag.Bool("v", false, "verbose")
)

// Use a mutex to prevent the commit poller and builders from using the primary
// local goroot simultaneously. Theoretically, Mercurial locks the repo when
// it's in use. Practically, it does a bad job of this.
// As a rule, only hold this lock while calling run or runLog.
var (
	goroot   string
	gorootMu sync.Mutex
)

var (
	binaryTagRe = regexp.MustCompile(`^(release\.r|weekly\.)[0-9\-.]+`)
	releaseRe   = regexp.MustCompile(`^release\.r[0-9\-.]+`)
	allCmd      = "all" + suffix
	cleanCmd    = "clean" + suffix
	suffix      = defaultSuffix()
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s goos-goarch...\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if len(flag.Args()) == 0 {
		flag.Usage()
	}
	goroot = filepath.Join(*buildroot, "goroot")
	builders := make([]*Builder, len(flag.Args()))
	for i, builder := range flag.Args() {
		b, err := NewBuilder(builder)
		if err != nil {
			log.Fatal(err)
		}
		builders[i] = b
	}

	if *failAll {
		failMode(builders)
		return
	}

	// set up work environment, use existing enviroment if possible
	if hgRepoExists(goroot) {
		log.Print("Found old workspace, will use it")
	} else {
		if err := os.RemoveAll(*buildroot); err != nil {
			log.Fatalf("Error removing build root (%s): %s", *buildroot, err)
		}
		if err := os.Mkdir(*buildroot, mkdirPerm); err != nil {
			log.Fatalf("Error making build root (%s): %s", *buildroot, err)
		}
		if err := hgClone(hgUrl, goroot); err != nil {
			log.Fatal("Error cloning repository:", err)
		}
	}

	// if specified, build revision and return
	if *buildRevision != "" {
		hash, err := fullHash(goroot, *buildRevision)
		if err != nil {
			log.Fatal("Error finding revision: ", err)
		}
		for _, b := range builders {
			if err := b.buildHash(hash); err != nil {
				log.Println(err)
			}
		}
		return
	}

	// Start commit watcher, and exit if that's all we're doing.
	if len(flag.Args()) == 0 {
		log.Print("no build targets specified; watching commits only")
		commitWatcher()
		return
	}
	go commitWatcher()

	// go continuous build mode (default)
	// check for new commits and build them
	for {
		built := false
		t := time.Now()
		if *parallel {
			done := make(chan bool)
			for _, b := range builders {
				go func(b *Builder) {
					done <- b.build()
				}(b)
			}
			for _ = range builders {
				built = <-done || built
			}
		} else {
			for _, b := range builders {
				built = b.build() || built
			}
		}
		// sleep if there was nothing to build
		if !built {
			time.Sleep(waitInterval)
		}
		// sleep if we're looping too fast.
		dt := time.Now().Sub(t)
		if dt < waitInterval {
			time.Sleep(waitInterval - dt)
		}
	}
}

// go continuous fail mode
// check for new commits and FAIL them
func failMode(builders []*Builder) {
	for {
		built := false
		for _, b := range builders {
			built = b.failBuild() || built
		}
		// stop if there was nothing to fail
		if !built {
			break
		}
	}
}

func NewBuilder(builder string) (*Builder, error) {
	b := &Builder{name: builder}

	// get goos/goarch from builder string
	s := strings.SplitN(builder, "-", 3)
	if len(s) >= 2 {
		b.goos, b.goarch = s[0], s[1]
	} else {
		return nil, fmt.Errorf("unsupported builder form: %s", builder)
	}

	// read keys from keyfile
	fn := ""
	if runtime.GOOS == "windows" {
		fn = os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
	} else {
		fn = os.Getenv("HOME")
	}
	fn = filepath.Join(fn, ".gobuildkey")
	if s := fn + "-" + b.name; isFile(s) { // builder-specific file
		fn = s
	}
	c, err := ioutil.ReadFile(fn)
	if err != nil {
		return nil, fmt.Errorf("readKeys %s (%s): %s", b.name, fn, err)
	}
	b.key = string(bytes.TrimSpace(bytes.SplitN(c, []byte("\n"), 2)[0]))
	return b, nil
}

// build checks for a new commit for this builder
// and builds it if one is found.
// It returns true if a build was attempted.
func (b *Builder) build() bool {
	hash, err := b.todo("build-go-commit", "", "")
	if err != nil {
		log.Println(err)
		return false
	}
	if hash == "" {
		return false
	}

	// Look for hash locally before running hg pull.
	if _, err := fullHash(goroot, hash[:12]); err != nil {
		// Don't have hash, so run hg pull.
		gorootMu.Lock()
		err = run(*cmdTimeout, nil, goroot, hgCmd("pull")...)
		gorootMu.Unlock()
		if err != nil {
			log.Println("hg pull failed:", err)
			return false
		}
	}

	err = b.buildHash(hash)
	if err != nil {
		log.Println(err)
	}
	return true
}

func (b *Builder) buildHash(hash string) error {
	log.Println(b.name, "building", hash)

	// create place in which to do work
	workpath := filepath.Join(*buildroot, b.name+"-"+hash[:12])
	if err := os.Mkdir(workpath, mkdirPerm); err != nil {
		return err
	}
	defer os.RemoveAll(workpath)

	// clone repo
	if err := hgClone(goroot, filepath.Join(workpath, "go")); err != nil {
		return err
	}

	// update to specified revision
	if err := run(*cmdTimeout, nil, filepath.Join(workpath, "go"), hgCmd("update", hash)...); err != nil {
		return err
	}

	srcDir := filepath.Join(workpath, "go", "src")

	// build
	logfile := filepath.Join(workpath, "build.log")
	cmd := *buildCmd
	if !filepath.IsAbs(cmd) {
		cmd = filepath.Join(srcDir, cmd)
	}
	startTime := time.Now()
	buildLog, status, err := runLog(*buildTimeout, b.envv(), logfile, srcDir, cmd)
	runTime := time.Now().Sub(startTime)
	if err != nil {
		return fmt.Errorf("%s: %s", *buildCmd, err)
	}

	if status != 0 {
		// record failure
		return b.recordResult(false, "", hash, "", buildLog, runTime)
	}

	// record success
	if err = b.recordResult(true, "", hash, "", "", runTime); err != nil {
		return fmt.Errorf("recordResult: %s", err)
	}

	// build Go sub-repositories
	goRoot := filepath.Join(workpath, "go")
	goPath := workpath
	b.buildSubrepos(goRoot, goPath, hash)

	return nil
}

// failBuild checks for a new commit for this builder
// and fails it if one is found.
// It returns true if a build was "attempted".
func (b *Builder) failBuild() bool {
	hash, err := b.todo("build-go-commit", "", "")
	if err != nil {
		log.Println(err)
		return false
	}
	if hash == "" {
		return false
	}

	log.Printf("fail %s %s\n", b.name, hash)

	if err := b.recordResult(false, "", hash, "", "auto-fail mode run by "+os.Getenv("USER"), 0); err != nil {
		log.Print(err)
	}
	return true
}

func (b *Builder) buildSubrepos(goRoot, goPath, goHash string) {
	for _, pkg := range dashboardPackages("subrepo") {
		// get the latest todo for this package
		hash, err := b.todo("build-package", pkg, goHash)
		if err != nil {
			log.Printf("buildSubrepos %s: %v", pkg, err)
			continue
		}
		if hash == "" {
			continue
		}

		// build the package
		if *verbose {
			log.Printf("buildSubrepos %s: building %q", pkg, hash)
		}
		buildLog, err := b.buildSubrepo(goRoot, goPath, pkg, hash)
		if err != nil {
			if buildLog == "" {
				buildLog = err.Error()
			}
			log.Printf("buildSubrepos %s: %v", pkg, err)
		}

		// record the result
		err = b.recordResult(err == nil, pkg, hash, goHash, buildLog, 0)
		if err != nil {
			log.Printf("buildSubrepos %s: %v", pkg, err)
		}
	}
}

// buildSubrepo fetches the given package, updates it to the specified hash,
// and runs 'go test -short pkg/...'. It returns the build log and any error.
func (b *Builder) buildSubrepo(goRoot, goPath, pkg, hash string) (string, error) {
	goTool := filepath.Join(goRoot, "bin", "go")
	env := append(b.envv(), "GOROOT="+goRoot, "GOPATH="+goPath)

	// add $GOROOT/bin and $GOPATH/bin to PATH
	for i, e := range env {
		const p = "PATH="
		if !strings.HasPrefix(e, p) {
			continue
		}
		sep := string(os.PathListSeparator)
		env[i] = p + filepath.Join(goRoot, "bin") + sep + filepath.Join(goPath, "bin") + sep + e[len(p):]
	}

	// fetch package and dependencies
	log, status, err := runLog(*cmdTimeout, env, "", goPath, goTool, "get", "-d", pkg+"/...")
	if err == nil && status != 0 {
		err = fmt.Errorf("go exited with status %d", status)
	}
	if err != nil {
		return log, err
	}

	// hg update to the specified hash
	pkgPath := filepath.Join(goPath, "src", pkg)
	if err := run(*cmdTimeout, nil, pkgPath, hgCmd("update", hash)...); err != nil {
		return "", err
	}

	// test the package
	log, status, err = runLog(*buildTimeout, env, "", goPath, goTool, "test", "-short", pkg+"/...")
	if err == nil && status != 0 {
		err = fmt.Errorf("go exited with status %d", status)
	}
	return log, err
}

// envv returns an environment for build/bench execution
func (b *Builder) envv() []string {
	if runtime.GOOS == "windows" {
		return b.envvWindows()
	}
	e := []string{
		"GOOS=" + b.goos,
		"GOARCH=" + b.goarch,
		"GOROOT_FINAL=/usr/local/go",
	}
	for _, k := range extraEnv {
		if s, ok := getenvOk(k); ok {
			e = append(e, k+"="+s)
		}
	}
	return e
}

// windows version of envv
func (b *Builder) envvWindows() []string {
	start := map[string]string{
		"GOOS":         b.goos,
		"GOARCH":       b.goarch,
		"GOROOT_FINAL": `c:\go`,
		"GOBUILDEXIT":  "1", // exit all.bat with completion status.
	}
	for _, name := range extraEnv {
		if s, ok := getenvOk(name); ok {
			start[name] = s
		}
	}
	skip := map[string]bool{
		"GOBIN":   true,
		"GOROOT":  true,
		"INCLUDE": true,
		"LIB":     true,
	}
	var e []string
	for name, v := range start {
		e = append(e, name+"="+v)
		skip[name] = true
	}
	for _, kv := range os.Environ() {
		s := strings.SplitN(kv, "=", 2)
		name := strings.ToUpper(s[0])
		switch {
		case name == "":
			// variables, like "=C:=C:\", just copy them
			e = append(e, kv)
		case !skip[name]:
			e = append(e, kv)
			skip[name] = true
		}
	}
	return e
}

func isDirectory(name string) bool {
	s, err := os.Stat(name)
	return err == nil && s.IsDir()
}

func isFile(name string) bool {
	s, err := os.Stat(name)
	return err == nil && !s.IsDir()
}

// commitWatcher polls hg for new commits and tells the dashboard about them.
func commitWatcher() {
	// Create builder just to get master key.
	b, err := NewBuilder("mercurial-commit")
	if err != nil {
		log.Fatal(err)
	}
	key := b.key

	for {
		if *verbose {
			log.Printf("poll...")
		}
		// Main Go repository.
		commitPoll(key, "")
		// Go sub-repositories.
		for _, pkg := range dashboardPackages("subrepo") {
			commitPoll(key, pkg)
		}
		if *verbose {
			log.Printf("sleep...")
		}
		time.Sleep(*commitInterval)
	}
}

func hgClone(url, path string) error {
	if url == goroot {
		gorootMu.Lock()
		defer gorootMu.Unlock()
	}
	return run(*cmdTimeout, nil, *buildroot, hgCmd("clone", url, path)...)
}

func hgRepoExists(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".hg"))
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// HgLog represents a single Mercurial revision.
type HgLog struct {
	Hash   string
	Author string
	Date   string
	Desc   string
	Parent string

	// Internal metadata
	added bool
}

// logByHash is a cache of all Mercurial revisions we know about,
// indexed by full hash.
var logByHash = map[string]*HgLog{}

// xmlLogTemplate is a template to pass to Mercurial to make
// hg log print the log in valid XML for parsing with xml.Unmarshal.
const xmlLogTemplate = `
	<Log>
	<Hash>{node|escape}</Hash>
	<Parent>{parent|escape}</Parent>
	<Author>{author|escape}</Author>
	<Date>{date|rfc3339date}</Date>
	<Desc>{desc|escape}</Desc>
	</Log>
`

// commitPoll pulls any new revisions from the hg server
// and tells the server about them.
func commitPoll(key, pkg string) {
	pkgRoot := goroot

	if pkg != "" {
		pkgRoot = filepath.Join(*buildroot, pkg)
		if !hgRepoExists(pkgRoot) {
			if err := hgClone(repoURL(pkg), pkgRoot); err != nil {
				log.Printf("%s: hg clone failed: %v", pkg, err)
				if err := os.RemoveAll(pkgRoot); err != nil {
					log.Printf("%s: %v", pkg, err)
				}
				return
			}
		}
	}

	lockGoroot := func() {
		if pkgRoot == goroot {
			gorootMu.Lock()
		}
	}
	unlockGoroot := func() {
		if pkgRoot == goroot {
			gorootMu.Unlock()
		}
	}

	lockGoroot()
	err := run(*cmdTimeout, nil, pkgRoot, hgCmd("pull")...)
	unlockGoroot()
	if err != nil {
		log.Printf("hg pull: %v", err)
		return
	}

	const N = 50 // how many revisions to grab

	lockGoroot()
	data, _, err := runLog(*cmdTimeout, nil, "", pkgRoot, hgCmd("log",
		"--encoding=utf-8",
		"--limit="+strconv.Itoa(N),
		"--template="+xmlLogTemplate)...,
	)
	unlockGoroot()
	if err != nil {
		log.Printf("hg log: %v", err)
		return
	}

	var logStruct struct {
		Log []HgLog
	}
	err = xml.Unmarshal([]byte("<Top>"+data+"</Top>"), &logStruct)
	if err != nil {
		log.Printf("unmarshal hg log: %v", err)
		return
	}

	logs := logStruct.Log

	// Pass 1.  Fill in parents and add new log entries to logsByHash.
	// Empty parent means take parent from next log entry.
	// Non-empty parent has form 1234:hashhashhash; we want full hash.
	for i := range logs {
		l := &logs[i]
		if l.Parent == "" && i+1 < len(logs) {
			l.Parent = logs[i+1].Hash
		} else if l.Parent != "" {
			l.Parent, _ = fullHash(pkgRoot, l.Parent)
		}
		if *verbose {
			log.Printf("hg log %s: %s < %s\n", pkg, l.Hash, l.Parent)
		}
		if logByHash[l.Hash] == nil {
			// Make copy to avoid pinning entire slice when only one entry is new.
			t := *l
			logByHash[t.Hash] = &t
		}
	}

	for i := range logs {
		l := &logs[i]
		addCommit(pkg, l.Hash, key)
	}
}

// addCommit adds the commit with the named hash to the dashboard.
// key is the secret key for authentication to the dashboard.
// It avoids duplicate effort.
func addCommit(pkg, hash, key string) bool {
	l := logByHash[hash]
	if l == nil {
		return false
	}
	if l.added {
		return true
	}

	// Check for already added, perhaps in an earlier run.
	if dashboardCommit(pkg, hash) {
		log.Printf("%s already on dashboard\n", hash)
		// Record that this hash is on the dashboard,
		// as must be all its parents.
		for l != nil {
			l.added = true
			l = logByHash[l.Parent]
		}
		return true
	}

	// Create parent first, to maintain some semblance of order.
	if l.Parent != "" {
		if !addCommit(pkg, l.Parent, key) {
			return false
		}
	}

	// Create commit.
	if err := postCommit(key, pkg, l); err != nil {
		log.Printf("failed to add %s to dashboard: %v", key, err)
		return false
	}
	return true
}

// fullHash returns the full hash for the given Mercurial revision.
func fullHash(root, rev string) (string, error) {
	if root == goroot {
		gorootMu.Lock()
	}
	s, _, err := runLog(*cmdTimeout, nil, "", root,
		hgCmd("log",
			"--encoding=utf-8",
			"--rev="+rev,
			"--limit=1",
			"--template={node}")...,
	)
	if root == goroot {
		gorootMu.Unlock()
	}
	if err != nil {
		return "", nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("cannot find revision")
	}
	if len(s) != 40 {
		return "", fmt.Errorf("hg returned invalid hash " + s)
	}
	return s, nil
}

var repoRe = regexp.MustCompile(`^code\.google\.com/p/([a-z0-9\-]+(\.[a-z0-9\-]+)?)(/[a-z0-9A-Z_.\-/]+)?$`)

// repoURL returns the repository URL for the supplied import path.
func repoURL(importPath string) string {
	m := repoRe.FindStringSubmatch(importPath)
	if len(m) < 2 {
		log.Printf("repoURL: couldn't decipher %q", importPath)
		return ""
	}
	return "https://code.google.com/p/" + m[1]
}

// defaultSuffix returns file extension used for command files in
// current os environment.
func defaultSuffix() string {
	if runtime.GOOS == "windows" {
		return ".bat"
	}
	return ".bash"
}

// defaultBuildRoot returns default buildroot directory.
func defaultBuildRoot() string {
	var d string
	if runtime.GOOS == "windows" {
		// will use c:\, otherwise absolute paths become too long
		// during builder run, see http://golang.org/issue/3358.
		d = `c:\`
	} else {
		d = os.TempDir()
	}
	return filepath.Join(d, "gobuilder")
}

func getenvOk(k string) (v string, ok bool) {
	v = os.Getenv(k)
	if v != "" {
		return v, true
	}
	keq := k + "="
	for _, kv := range os.Environ() {
		if kv == keq {
			return "", true
		}
	}
	return "", false
}

func hgCmd(args ...string) []string {
	return append([]string{"hg", "--config", "extensions.codereview=!"}, args...)
}
