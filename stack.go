package tracepanel

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// This module's path: its own frames are never the app's.
const modulePath = "github.com/tracepanel/tracepanel-go"

// appModule is the app's module path: frames under it (and the main package's) are the app's own code.
var appModule atomic.Pointer[string]

// inApp: the app's own code, its main package or its module's packages (Tracepanel's own excepted, but for its
// tests, which play the app).
func inApp(function, file string) bool {
	if strings.HasPrefix(function, modulePath+".") || strings.HasPrefix(function, modulePath+"/") {
		return strings.HasSuffix(file, "_test.go")
	}
	if strings.HasPrefix(function, "main.") {
		return true
	}
	module := appModule.Load()
	return module != nil && *module != "" &&
		(strings.HasPrefix(function, *module+"/") || strings.HasPrefix(function, *module+"."))
}

func callers(skip int) []uintptr {
	pcs := make([]uintptr, 64)
	return pcs[:runtime.Callers(skip+1, pcs)]
}

// origin is `file:line in function` of the innermost frame of the app's own code on the stack: the line that ran a
// query, made a call, started a task.
func origin() string {
	frames := runtime.CallersFrames(callers(2))
	for {
		frame, more := frames.Next()
		if inApp(frame.Function, frame.File) {
			return fmt.Sprintf("%s:%d in %s", relative(frame.File), frame.Line, shortFunction(frame.Function))
		}
		if !more {
			return ""
		}
	}
}

// relative is a file of the project as the project sees it, from the working directory on, in forward slashes on
// every OS (Go names a frame's file in forward slashes, D:/app/main.go on Windows, where the working directory is
// D:\app); a file elsewhere (the standard library's, a module's) stays whole.
func relative(file string) string {
	wd, err := os.Getwd()
	if err != nil {
		return file
	}
	return relativeTo(file, wd, runtime.GOOS == "windows")
}

func relativeTo(file, wd string, windows bool) string {
	if windows {
		file = strings.ReplaceAll(file, `\`, "/")
		wd = strings.ReplaceAll(wd, `\`, "/")
	}
	prefix := strings.TrimSuffix(wd, "/") + "/"
	if len(file) <= len(prefix) {
		return file
	}
	// Windows' paths are the same in any case: D:/app and d:/App.
	if file[:len(prefix)] == prefix || windows && strings.EqualFold(file[:len(prefix)], prefix) {
		return file[len(prefix):]
	}
	return file
}

// shortFunction is "(*App).listBooks" of "example.com/shop.(*App).listBooks".
func shortFunction(function string) string {
	if slash := strings.LastIndex(function, "/"); slash >= 0 {
		function = function[slash+1:]
	}
	if dot := strings.Index(function, "."); dot >= 0 {
		return function[dot+1:]
	}
	return function
}

func splitFunction(function string) (string, string) {
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return function, function
	}
	return function[:slash+1+dot], function[slash+1+dot+1:]
}

// recordPanic adds a panic to its trace, with the stack it was raised in: `pcs` as recover() sees it.
func recordPanic(rec *recording, recovered any, pcs []uintptr) {
	frames := []frameDoc{}
	callers := runtime.CallersFrames(pcs)
	// The stack seen from recover() starts with the deferred function and the panic machinery: from the frame below
	// runtime.gopanic on, it is where the panic happened.
	panicking := false
	for {
		frame, more := callers.Next()
		if panicking {
			module, function := splitFunction(frame.Function)
			own := inApp(frame.Function, frame.File)
			start, context := sourceAround(frame.File, frame.Line, own)
			frames = append(frames, frameDoc{
				Filename: relative(frame.File), Module: module, Function: function, Lineno: frame.Line,
				InApp: own, ContextStart: start, Context: context,
			})
		}
		panicking = panicking || frame.Function == "runtime.gopanic"
		if !more {
			break
		}
	}
	// Outermost first, as the console shows every SDK's frames.
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	rec.addError(errorDoc{
		SpanID: ptr(rec.current()), Time: micros(time.Now()), Type: fmt.Sprintf("panic(%T)", recovered),
		Message: fmt.Sprint(recovered), Frames: frames, Chain: []any{},
	})
}

// goVersion is "1.27.1" (runtime.Version() says "go1.27.1").
func goVersion() string { return strings.TrimPrefix(runtime.Version(), "go") }

const contextLines = 5

// sourceAround is the source around a line of the app's own code, which the console shows with the frame.
func sourceAround(file string, line int, own bool) (*int, []string) {
	if !own {
		return nil, []string{}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, []string{}
	}
	lines := strings.Split(string(data), "\n")
	start := max(line-contextLines, 1)
	end := min(line+contextLines, len(lines))
	if start > end {
		return nil, []string{}
	}
	return &start, lines[start-1 : end]
}
