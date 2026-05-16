package contextmode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Language enumerates the script runtimes execute() supports. Bash is
// the default; other entries are matched on a literal language string
// in the request OR by shebang on the script itself.
type Language string

const (
	LangBash   Language = "bash"
	LangPython Language = "python"
	LangNode   Language = "node"
	LangGo     Language = "go"
)

// ValidLanguages is the set surfaced in tool descriptions and
// validated against incoming requests.
var ValidLanguages = map[Language]bool{
	LangBash:   true,
	LangPython: true,
	LangNode:   true,
	LangGo:     true,
}

// Detect picks a language from the user's hint OR the script's shebang.
// Empty hint + no shebang → LangBash.
func Detect(hint string, script string) Language {
	hint = strings.ToLower(strings.TrimSpace(hint))
	switch hint {
	case "bash", "sh", "shell":
		return LangBash
	case "python", "py", "python3":
		return LangPython
	case "node", "js", "javascript":
		return LangNode
	case "go", "golang":
		return LangGo
	}
	// Shebang: only the FIRST line matters.
	if strings.HasPrefix(script, "#!") {
		first := script
		if i := strings.IndexByte(script, '\n'); i > 0 {
			first = script[:i]
		}
		switch {
		case strings.Contains(first, "bash"), strings.Contains(first, "/sh"):
			return LangBash
		case strings.Contains(first, "python"):
			return LangPython
		case strings.Contains(first, "node"):
			return LangNode
		}
	}
	return LangBash
}

// Command resolves a Language into a binary path and the args to run a
// script piped on stdin. Returning stdinScript=true means the caller
// passes the script via stdin; false means it goes via a temp file.
//
// We prefer stdin (no temp file mess) unless the runtime can't read
// from stdin sensibly — go especially needs `go run -` which IS stdin.
func Command(l Language) (binary string, args []string, stdinScript bool, err error) {
	switch l {
	case LangBash:
		bin := lookFirst([]string{"bash", "sh"}, "BASH")
		if bin == "" {
			return "", nil, false, errors.New("bash not found in PATH")
		}
		// `-s` reads script from stdin.
		return bin, []string{"-s"}, true, nil
	case LangPython:
		bin := lookFirst([]string{"python3", "python"}, "PYTHON")
		if bin == "" {
			return "", nil, false, errors.New("python not found in PATH")
		}
		// `-` reads script from stdin.
		return bin, []string{"-"}, true, nil
	case LangNode:
		bin := lookFirst([]string{"node", "nodejs"}, "NODE")
		if bin == "" {
			return "", nil, false, errors.New("node not found in PATH")
		}
		// `-e -` doesn't work cleanly; we pipe via stdin and use `--`.
		// Node reads stdin when given no script and `--`.
		return bin, []string{"--"}, true, nil
	case LangGo:
		bin := lookFirst([]string{"go"}, "GO")
		if bin == "" {
			return "", nil, false, errors.New("go not found in PATH")
		}
		// `go run -` reads source from stdin.
		return bin, []string{"run", "-"}, true, nil
	default:
		return "", nil, false, fmt.Errorf("unsupported language %q", l)
	}
}

// lookFirst tries each candidate via PATH and an env override. The env
// var lets a user pin `GOGENTS_PYTHON=/opt/python3.12/bin/python3` etc.
func lookFirst(candidates []string, envBase string) string {
	if envBase != "" {
		if p := os.Getenv("GOGENTS_" + envBase); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
