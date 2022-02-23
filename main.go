package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const version = "1.0.2-snapshot"

var exit = os.Exit

func main() {
	exit(gobcoMain(os.Stdout, os.Stderr, os.Args...))
}

func gobcoMain(stdout, stderr io.Writer, args ...string) int {
	g := newGobco(stdout, stderr)
	g.parseCommandLine(args)
	g.prepareTmp()
	g.instrument()
	g.runGoTest()
	g.printOutput()
	g.cleanUp()
	return g.exitCode
}

type gobco struct {
	listAll     bool
	immediately bool
	keep        bool
	coverTest   bool

	goTestArgs []string
	args       []argInfo

	statsFilename string

	exitCode int

	logger
	buildEnv
}

func newGobco(stdout io.Writer, stderr io.Writer) *gobco {
	var g gobco
	g.logger.init(stdout, stderr)
	g.buildEnv.init(&g.logger)
	return &g
}

func (g *gobco) parseCommandLine(argv []string) {
	args := g.parseOptions(argv)
	g.parseArgs(args)
}

func (g *gobco) parseOptions(argv []string) []string {
	var help, ver bool

	flags := flag.NewFlagSet(filepath.Base(argv[0]), flag.ContinueOnError)
	flags.BoolVar(&help, "help", false,
		"print the available command line options")
	flags.BoolVar(&g.immediately, "immediately", false,
		"persist the coverage immediately at each check point")
	flags.BoolVar(&g.keep, "keep", false,
		"don't remove the temporary working directory")
	flags.BoolVar(&g.listAll, "list-all", false,
		"at finish, print also those conditions that are fully covered")
	flags.StringVar(&g.statsFilename, "stats", "",
		"load and persist the JSON coverage data to this `file`")
	flags.Var(newSliceFlag(&g.goTestArgs), "test",
		"pass the `option` to \"go test\", such as -vet=off")
	flags.BoolVar(&g.verbose, "verbose", false,
		"show progress messages")
	flags.BoolVar(&g.coverTest, "cover-test", false,
		"cover the test code as well")
	flags.BoolVar(&ver, "version", false,
		"print the gobco version")

	flags.SetOutput(g.stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(),
			"usage: %s [options] package...\n", flags.Name())
		flags.PrintDefaults()
		g.exitCode = 2
	}

	err := flags.Parse(argv[1:])
	if g.exitCode != 0 {
		exit(g.exitCode)
	}
	g.check(err)

	if help {
		flags.SetOutput(g.stdout)
		flags.Usage()
		exit(0)
	}

	if ver {
		g.outf("%s", version)
		exit(0)
	}

	return flags.Args()
}

func (g *gobco) parseArgs(args []string) {
	if len(args) == 0 {
		args = []string{"."}
	}

	if len(args) > 1 {
		panic("gobco: checking multiple packages doesn't work yet")
	}

	for _, arg := range args {
		arg = filepath.FromSlash(arg)
		g.args = append(g.args, g.classify(arg))
	}
}

// classify determines how to handle the argument, depending on whether it is
// a single file or directory, and whether it is located in a Go module or not.
func (g *gobco) classify(arg string) argInfo {
	st, err := os.Stat(arg)
	isDir := err == nil && st.IsDir()

	dir := arg
	base := ""
	if !isDir {
		dir = filepath.Dir(dir)
		base = filepath.Base(arg)
	}

	if moduleRoot, moduleRel := g.findInModule(dir); moduleRoot != "" {
		copyDst := "module-" + randomHex(8) // Must be outside 'gopath/'.
		packageDir := filepath.Join(copyDst, moduleRel)
		return argInfo{
			arg:       arg,
			argDir:    dir,
			module:    true,
			copySrc:   moduleRoot,
			copyDst:   copyDst,
			instrFile: base,
			instrDir:  packageDir,
		}
	}

	if relDir := g.findInGopath(dir); relDir != "" {
		relDir := filepath.Join("gopath", relDir)
		return argInfo{
			arg:       arg,
			argDir:    dir,
			module:    false,
			copySrc:   dir,
			copyDst:   relDir,
			instrFile: base,
			instrDir:  relDir,
		}
	}

	g.check(fmt.Errorf("error: argument %q must be inside GOPATH", arg))
	panic("unreachable")
}

// findInGopath returns the directory relative to the enclosing GOPATH, if any.
func (g *gobco) findInGopath(arg string) string {
	gopaths := g.gopaths()

	abs, err := filepath.Abs(arg)
	g.check(err)

	for _, gopath := range filepath.SplitList(gopaths) {

		rel, err := filepath.Rel(gopath, abs)
		g.check(err)

		if strings.HasPrefix(rel, "src") {
			return rel
		}
	}
	return ""
}

func (g *gobco) gopaths() string {
	gopaths := os.Getenv("GOPATH")
	if gopaths != "" {
		return gopaths
	}

	home, err := os.UserHomeDir()
	g.check(err)
	return filepath.Join(home, "go")
}

func (g *gobco) findInModule(dir string) (moduleRoot, moduleRel string) {
	absDir, err := filepath.Abs(dir)
	g.check(err)

	abs := absDir
	for {
		if _, err := os.Lstat(filepath.Join(abs, "go.mod")); err == nil {
			rel, err := filepath.Rel(abs, absDir)
			g.check(err)

			root := abs
			if rel == "." {
				root = dir
			}

			return root, rel
		}

		parent := filepath.Dir(abs)
		if parent == abs {
			return "", ""
		}
		abs = parent
	}
}

// prepareTmp copies the source files to the temporary directory.
//
// Some of these files will later be overwritten by gobco.instrumenter.
func (g *gobco) prepareTmp() {
	if g.statsFilename != "" {
		var err error
		g.statsFilename, err = filepath.Abs(g.statsFilename)
		g.check(err)
	} else {
		g.statsFilename = g.file("gobco-counts.json")
	}

	// TODO: Research how "package/..." is handled by other go commands.
	for _, arg := range g.args {
		dstDir := g.file(arg.copyDst)
		g.check(copyDir(arg.copySrc, dstDir))
	}
}

func (g *gobco) instrument() {
	var in instrumenter
	in.immediately = g.immediately
	in.listAll = g.listAll
	in.coverTest = g.coverTest

	for _, arg := range g.args {
		instrDst := g.file(arg.instrDir)
		in.instrument(arg.argDir, arg.instrFile, instrDst)
		g.verbosef("Instrumented %s to %s", arg.arg, instrDst)
	}
}

func (g *gobco) runGoTest() {
	for _, arg := range g.args {
		gopaths := ""
		if !arg.module {
			gopaths = g.gopaths()
		}
		g.exitCode = goTest{}.run(
			arg,
			g.goTestArgs,
			g.verbose,
			gopaths,
			g.statsFilename,
			&g.buildEnv,
		)
	}
}

func (g *gobco) printOutput() {
	conds := g.load(g.statsFilename)

	funcCondsCount := map[string]int{}
	funcReachedCount := map[string]int{}
	cnt := 0
	for _, c := range conds {
		funcName := c.Func

		if _, ok := funcCondsCount[funcName]; !ok {
			funcCondsCount[funcName] = 0
		}
		funcCondsCount[funcName]++

		if _, ok := funcReachedCount[funcName]; !ok {
			funcReachedCount[funcName] = 0
		}

		if c.TrueCount > 0 {
			cnt++
			funcReachedCount[funcName]++
		}
		if c.FalseCount > 0 {
			cnt++
			funcReachedCount[funcName]++
		}
	}

	g.outf("")
	g.outf("Overall branch coverage: %.2f%% (%d/%d)", float64(cnt)/float64(len(conds)*2), cnt, len(conds)*2)

	g.outf("")
	g.outf("Per function coverage:")
	for k, v := range funcCondsCount {
		g.outf("%s %.2f%% (%d/%d)", k, (float64(funcReachedCount[k])/(float64(v)*2))*100, funcReachedCount[k], v*2)
	}

	g.outf("")
	g.outf("Improvements can be done:")

	for _, cond := range conds {
		g.printCond(cond)
	}
}

func (g *gobco) cleanUp() {
	if g.keep {
		g.errf("")
		g.errf("the temporary files are in %s", g.tmpdir)
	} else {
		err := os.RemoveAll(g.tmpdir)
		if err != nil {
			g.verbosef("%s", err)
		}
	}
}

func (g *gobco) load(filename string) []condition {
	file, err := os.Open(filename)
	g.check(err)

	defer func() {
		closeErr := file.Close()
		g.check(closeErr)
	}()

	var data []condition
	decoder := json.NewDecoder(bufio.NewReader(file))
	decoder.DisallowUnknownFields()
	g.check(decoder.Decode(&data))

	return data
}

func (g *gobco) printCond(cond condition) {

	trueCount := cond.TrueCount
	falseCount := cond.FalseCount
	start := cond.Start
	code := cond.Code

	if !g.listAll && trueCount > 0 && falseCount > 0 {
		return
	}

	capped := func(count int) int {
		if count > 1 {
			return 2
		}
		if count == 1 {
			return 1
		}
		return 0
	}

	switch 3*capped(trueCount) + capped(falseCount) {
	case 0:
		g.outf("%s: condition %q was never evaluated",
			start, code)
	case 1:
		g.outf("%s: condition %q was once false but never true",
			start, code)
	case 2:
		g.outf("%s: condition %q was %d times false but never true",
			start, code, falseCount)
	case 3:
		g.outf("%s: condition %q was once true but never false",
			start, code)
	case 4:
		g.outf("%s: condition %q was once true and once false",
			start, code)
	case 5:
		g.outf("%s: condition %q was once true and %d times false",
			start, code, falseCount)
	case 6:
		g.outf("%s: condition %q was %d times true but never false",
			start, code, trueCount)
	case 7:
		g.outf("%s: condition %q was %d times true and once false",
			start, code, trueCount)
	default:
		g.outf("%s: condition %q was %d times true and %d times false",
			start, code, trueCount, falseCount)
	}
}

// goTest groups the functions that run 'go test' with the proper arguments.
type goTest struct{}

func (t goTest) run(
	arg argInfo,
	extraArgs []string,
	verbose bool,
	gopaths string,
	statsFilename string,
	e *buildEnv,
) int {
	args := t.args(verbose, extraArgs)
	goTest := exec.Command("go", args[1:]...)
	goTest.Stdout = e.stdout
	goTest.Stderr = e.stderr
	goTest.Dir = e.file(arg.instrDir)
	goTest.Env = t.env(e.tmpdir, gopaths, statsFilename)

	cmdline := strings.Join(args, " ")
	e.verbosef("Running %q in %q", cmdline, goTest.Dir)

	err := goTest.Run()
	if err != nil {
		e.errf("%s", err)
		return 1
	} else {
		e.verbosef("Finished %s", cmdline)
		return 0
	}
}

func (goTest) args(verbose bool, extraArgs []string) []string {
	args := []string{"go", "test"}

	if verbose {
		// The -v is necessary to produce any output at all.
		// Without it, most of the log output is suppressed.
		args = append(args, "-v")
	}

	// Work around test result caching which does not apply anyway,
	// since the instrumented files are written to a new directory
	// each time.
	//
	// Without this option, "go test" sometimes needs twice the time.
	args = append(args, "-test.count", "1")

	args = append(args, ".")

	// 'go test' allows flags even after packages.
	args = append(args, extraArgs...)

	return args
}

func (goTest) env(tmpdir, gopaths, statsFilename string) []string {

	var env []string

	for _, envVar := range os.Environ() {
		if gopaths == "" && strings.HasPrefix(envVar, "GOPATH=") {
			continue
		}
		env = append(env, envVar)
	}

	if gopaths != "" {
		gopathDir := filepath.Join(tmpdir, "gopath")
		gopath := gopathDir + string(filepath.ListSeparator) + gopaths
		env = append(env, "GOPATH="+gopath)
		env = append(env, "GO111MODULE=off")
	}

	env = append(env, "GOBCO_STATS="+statsFilename)

	return env
}

// buildEnv describes the environment in which all interesting pieces of code
// are collected and instrumented.
type buildEnv struct {
	tmpdir string
	*logger
}

func (e *buildEnv) init(r *logger) {

	tmpdir := filepath.Join(os.TempDir(), "gobco-"+randomHex(8))

	r.check(os.MkdirAll(tmpdir, 0777))

	r.verbosef("The temporary working directory is %s", tmpdir)

	*e = buildEnv{tmpdir, r}
}

// file returns the absolute path of the given path, which is interpreted
// relative to the temporary directory.
func (e *buildEnv) file(rel string) string {
	return filepath.Join(e.tmpdir, filepath.FromSlash(rel))
}

// logger provides basic logging and error checking.
type logger struct {
	stdout  io.Writer
	stderr  io.Writer
	verbose bool
}

func (l *logger) init(stdout io.Writer, stderr io.Writer) {
	l.stdout = stdout
	l.stderr = stderr
}

func (l *logger) check(err error) {
	if err != nil {
		l.errf("%s", err)
		exit(1)
	}
}

func (l *logger) outf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(l.stdout, format+"\n", args...)
}

func (l *logger) errf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(l.stderr, format, args...)
}

func (l *logger) verbosef(format string, args ...interface{}) {
	if l.verbose {
		l.errf(format, args...)
	}
}

// argInfo describes the properties of an item that will be instrumented.
//
// If it is inside GOPATH, it or its containing directory is copied, otherwise
// the whole Go module will be copied.
//
// If it is a file, only that file is instrumented, otherwise the whole package
// is instrumented. Even in case of a single file, the whole directory is
// copied though.
type argInfo struct {
	// From the command line, using either '/' or '\\' as separator.
	arg string

	// Either arg if it is a directory, or its containing directory.
	// Either absolute, or relative to the current working directory.
	//
	// This is the directory from which the code is instrumented. The paths
	// to the files in this directory will end up in the coverage output.
	argDir string

	// Whether arg is a module (true) or a traditional package (false).
	module bool

	// The directory that will be copied to the build environment.
	// Either absolute, or relative to the current working directory.
	// For modules, it is the module root, so that go.mod is copied as well.
	// For other packages it is the package directory itself.
	copySrc string

	// The copy destination, relative to tmpdir.
	// For modules, it is some directory outside 'gopath/src',
	// traditional packages are copied to 'gopath/src/$pkgname'.
	copyDst string

	// The single file in which to instrument the code, relative to instrDir,
	// or "" to instrument the whole package.
	instrFile string

	// The directory where the instrumented code is saved, relative to tmpdir.
	// The directory in which to run 'go test', relative to tmpdir.
	instrDir string
}

type condition struct {
	Start      string
	Code       string
	TrueCount  int
	FalseCount int
	Func       string
}
