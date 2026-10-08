package bench

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// SampleRSS returns pid's resident set size in bytes: /proc on Linux, ps on
// macOS, an error elsewhere (callers treat RSS as optional).
func SampleRSS(pid int) (int64, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return 0, err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "VmRSS:") {
				fields := strings.Fields(sc.Text())
				if len(fields) >= 2 {
					kb, err := strconv.ParseInt(fields[1], 10, 64)
					if err != nil {
						return 0, err
					}
					return kb * 1024, nil
				}
			}
		}
		return 0, errors.New("VmRSS not found")
	case "darwin":
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, err
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	default:
		return 0, fmt.Errorf("rss sampling unsupported on %s", runtime.GOOS)
	}
}
