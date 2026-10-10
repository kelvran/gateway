package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// projectLocalSettings is the one project-scope file connect writes; it must
// be gitignored (the connect page: Claude Code adds it to the global gitignore
// when it writes there; a hand-written one must be ignored first).
const projectLocalSettings = ".claude/settings.local.json"

// gitRunner lets tests stand in for the git binary; nil runs the real one.
var gitRunner func(dir string, args ...string) (exitCode int, err error)

// gitEnvDropped are the variables that redirect git's repository discovery.
// A hook or git-* script exports them, and a git that inherited them would
// answer connect's question for another repository than the one cwd is in.
// GIT_CONFIG_* stay (the tests isolate the global config through them).
var gitEnvDropped = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES"}

func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !slices.ContainsFunc(gitEnvDropped, func(name string) bool { return strings.HasPrefix(kv, name+"=") }) {
			out = append(out, kv)
		}
	}
	return out
}

func runGit(dir string, args ...string) (int, error) {
	if gitRunner != nil {
		return gitRunner(dir, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the argv is fixed by the two callers (check-ignore and ls-files on a constant or a base name); only cwd varies
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = nil, nil
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// projectWriteAllowed applies decision 8's rule for --scope project, with cwd
// = dir: `git check-ignore -q -- .claude/settings.local.json` honours every
// ignore source (.gitignore at any level, .git/info/exclude, core.excludesFile
// — the file Claude Code itself writes to), unlike a textual read of one
// .gitignore. Exit 0 → write. Exit 1 → `git ls-files --error-unmatch`: tracked
// → refuse with the `git rm --cached` remedy; otherwise refuse with the ignore
// remedy. Exit 128 is two cases git does not distinguish: with no .git here or
// above it is "not a work tree" → write with a note; with one, git could not
// read the repository that is there (a corrupt .git, dubious ownership,
// GIT_CEILING_DIRECTORIES) → refuse. git absent → refuse when a .git exists
// here or above, else write with a note. --force is the one override and
// always writes, with a note.
func projectWriteAllowed(dir string, force bool) (note string, err error) {
	if _, lookErr := exec.LookPath("git"); lookErr != nil && gitRunner == nil {
		if hasGitDir(dir) {
			if force {
				return "git is not on PATH, so gitignore coverage of " + projectLocalSettings + " could not be proven; written because of --force", nil
			}
			return "", runtimeErr("git is not on PATH and %s is inside a repository, so gitignore coverage of %s cannot be proven; add `%s` to .gitignore and pass --force", dir, projectLocalSettings, projectLocalSettings)
		}
		return "no .git here or above: gitignore coverage of " + projectLocalSettings + " was not checked", nil
	}
	code, runErr := runGit(dir, "check-ignore", "-q", "--", projectLocalSettings)
	if runErr != nil {
		return "", runtimeErr("running git check-ignore: %v", runErr)
	}
	switch code {
	case 0:
		return "", nil
	case 128:
		if !hasGitDir(dir) {
			return "not inside a git work tree: nothing can be committed from here", nil
		}
		if force {
			return "git could not read this repository (exit 128), so gitignore coverage of " + projectLocalSettings + " could not be proven; written because of --force", nil
		}
		return "", runtimeErr("git could not read this repository (exit 128: a corrupt or unreadable .git, dubious ownership, or GIT_CEILING_DIRECTORIES), so gitignore coverage of %s cannot be proven; fix the repository or pass --force", projectLocalSettings)
	case 1:
		tracked, runErr := runGit(dir, "ls-files", "--error-unmatch", "--", projectLocalSettings)
		if runErr != nil {
			return "", runtimeErr("running git ls-files: %v", runErr)
		}
		if tracked == 0 {
			if force {
				return projectLocalSettings + " is TRACKED by git and was written because of --force; run `git rm --cached " + projectLocalSettings + "` and add it to .gitignore before committing", nil
			}
			return "", runtimeErr("%s is tracked by git, so the key would be committed; run `git rm --cached %s`, add `%s` to .gitignore, and re-run (or pass --force)", projectLocalSettings, projectLocalSettings, projectLocalSettings)
		}
		if force {
			return projectLocalSettings + " is not gitignored and was written because of --force; add it to .gitignore before committing", nil
		}
		return "", runtimeErr("%s is not gitignored, so the key could be committed; add it — `printf '%%s\\n' '%s' >> .gitignore` (or your core.excludesFile) — and re-run, or pass --force", projectLocalSettings, projectLocalSettings)
	default:
		return "", runtimeErr("git check-ignore exited %d", code)
	}
}

// trackedWriteGuard asks git, at the file's real location (every directory
// symlink resolved, so a ~/.claude that stow links into a dotfiles repository
// is seen), whether the settings file is tracked — a tracked file would
// commit the key. Tracked → refuse unless --force. Exit 128 with a .git at or
// above the location → git could not read that repository → refuse unless
// --force; with none → not a repository, write. git absent → not checked
// (project scope has its own rule for that). A bare-repository manager git
// reaches only through GIT_DIR (yadm) is not seen: runGit drops it.
func trackedWriteGuard(path string, force bool) (note string, err error) {
	if _, lookErr := exec.LookPath("git"); lookErr != nil && gitRunner == nil {
		return "", nil
	}
	runDir, rel := nearestExisting(resolveDir(filepath.Dir(path)), filepath.Base(path))
	code, runErr := runGit(runDir, "ls-files", "--error-unmatch", "--", rel)
	if runErr != nil {
		return "", runtimeErr("running git ls-files: %v", runErr)
	}
	switch code {
	case 0:
		if force {
			return path + " is TRACKED by git and was written because of --force; run `git rm --cached " + rel + "` in " + runDir + " before committing", nil
		}
		return "", runtimeErr("%s is tracked by git (the repository at or above %s), so the key would be committed; run `git rm --cached %s` there and re-run, or pass --force", path, runDir, rel)
	case 1:
		return "", nil
	case 128:
		if !hasGitDir(runDir) {
			return "", nil
		}
		if force {
			return "git could not read the repository at or above " + runDir + " (exit 128), so whether " + path + " is tracked is unknown; written because of --force", nil
		}
		return "", runtimeErr("git could not read the repository at or above %s (exit 128: a corrupt or unreadable .git, dubious ownership, or GIT_CEILING_DIRECTORIES), so whether %s is tracked cannot be checked; fix the repository or pass --force", runDir, path)
	default:
		return "", runtimeErr("git ls-files exited %d", code)
	}
}

// resolveDir resolves every symbolic link in dir, tolerating a tail that does
// not exist yet: the nearest existing ancestor is resolved and the missing
// tail re-joined.
func resolveDir(dir string) string {
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{real}, tail...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(append([]string{dir}, tail...)...)
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
}

// nearestExisting returns the deepest existing directory at or above dir and
// the path of dir/name relative to it, so git can be asked about a file whose
// directory is not created yet.
func nearestExisting(dir, name string) (runDir, rel string) {
	rel = name
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir, rel
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, rel
		}
		rel = filepath.Join(filepath.Base(dir), rel)
		dir = parent
	}
}

// hasGitDir reports whether dir or an ancestor holds a .git entry.
func hasGitDir(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
