//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Only the standard library's syscall package is used, so the project has
// no third-party dependencies.

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	mpr                          = syscall.NewLazyDLL("mpr.dll")
	procWNetAddConnection2W      = mpr.NewProc("WNetAddConnection2W")
)

const (
	createNoWindow                  = 0x08000000
	jobObjectExtendedLimitInfoClass = 9
	jobObjectLimitKillOnJobClose    = 0x00002000
	processSetQuota                 = 0x0100
	processTerminate                = 0x0001
	resourceTypeDisk                = 1
	errorSessionCredentialConflict  = 1219
	errorAlreadyAssigned            = 85
)

// programDataDir is the classic install location, and where the pointer to
// the current install location is kept.
func programDataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "ffagent")
}

// locationFile records where ffagent is installed when that isn't
// programDataDir (normally <compose folder>\ffagent).
func locationFile() string { return filepath.Join(programDataDir(), "install-dir.txt") }

var (
	dataDirOnce sync.Once
	dataDirVal  string
)

// dataDir is the folder holding ffagent.conf, the log and downloaded ffmpeg:
// FFAGENT_HOME, else the folder of this exe if it holds a config (the
// installed copy), else the recorded install location, else ProgramData.
func dataDir() string {
	dataDirOnce.Do(func() {
		if d := os.Getenv("FFAGENT_HOME"); d != "" {
			dataDirVal = d
			return
		}
		if exe, err := os.Executable(); err == nil {
			if d := filepath.Dir(exe); fileExistsAt(filepath.Join(d, "ffagent.conf")) {
				dataDirVal = d
				return
			}
		}
		if b, err := os.ReadFile(locationFile()); err == nil {
			if d := strings.TrimSpace(string(b)); d != "" && fileExistsAt(filepath.Join(d, "ffagent.conf")) {
				dataDirVal = d
				return
			}
		}
		dataDirVal = programDataDir()
	})
	return dataDirVal
}

// ---- Elevation -----------------------------------------------------------

var (
	shell32           = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// relaunchElevated starts this exe again with args in a new Administrator
// window (Windows shows the UAC prompt) and returns. The new window stays
// open until Enter is pressed, see pauseIfOwnConsole.
func relaunchElevated(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	cwd, _ := os.Getwd()
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	dir, _ := syscall.UTF16PtrFromString(cwd)
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)), uintptr(unsafe.Pointer(dir)), 1 /* SW_SHOWNORMAL */)
	if r <= 32 {
		if r == 5 {
			return errors.New("administrator rights are needed (the Windows prompt was declined)")
		}
		return fmt.Errorf("could not start as administrator (error %d); run it from Terminal (Admin)", r)
	}
	fmt.Println("Continuing in a new Administrator window ...")
	skipPause = true // this window has nothing more to show
	return nil
}

// noArgs handles starting ffagent without arguments (double-click): set up
// for the compose file next to the exe, or show the status of an install.
func noArgs() int {
	exe, err := os.Executable()
	if err == nil {
		if f := findComposeFile(filepath.Dir(exe)); f != "" {
			fmt.Println("Found", f)
			return dispatch("install", []string{"--compose", f})
		}
	}
	if fileExistsAt(filepath.Join(dataDir(), "ffagent.conf")) {
		return dispatch("status", nil)
	}
	fmt.Printf(usageText, agentVersion, dataDir())
	fmt.Println("\nTip: put ffagent.exe next to your Jellyfin docker-compose.yml and double-click it.")
	return 0
}

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// ---- Job object: every ffmpeg dies with the agent --------------------------

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobGroup interface{ Add(*os.Process) error }

type winJob struct{ h syscall.Handle }

func newJobGroup() (jobGroup, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := jobExtendedLimit{Basic: jobBasicLimit{LimitFlags: jobObjectLimitKillOnJobClose}}
	r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInfoClass,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	// The handle is intentionally never closed: when the agent exits, Windows
	// closes it and kills every ffmpeg still in the job.
	return &winJob{h: syscall.Handle(h)}, nil
}

func (j *winJob) Add(p *os.Process) error {
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(ph)
	r, _, err := procAssignProcessToJobObject.Call(uintptr(j.h), uintptr(ph))
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// ---- Path mirroring: C:\media6 -> D:\Videos\Movies -------------------------

func linkPath(drive, container string) string {
	return drive + filepath.FromSlash(container)
}

func ensureLinks(cfg *Config) []PathStatus {
	shareErr := connectShares(cfg.Credentials)
	out := make([]PathStatus, 0, len(cfg.Paths))
	for _, p := range cfg.Paths {
		st := PathStatus{Container: p.Container, Link: linkPath(cfg.Drive, p.Container), Target: p.Host}
		if err := ensureLink(st.Link, p.Host); err != nil {
			st.Error = err.Error()
			for share, serr := range shareErr {
				if strings.HasPrefix(strings.ToLower(p.Host), strings.ToLower(share)) {
					st.Error = "log on to " + share + " failed: " + serr.Error() + "; " + st.Error
				}
			}
		} else {
			st.OK = true
		}
		out = append(out, st)
	}
	return out
}

func ensureLink(link, target string) error {
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("Windows folder %s is not reachable: %v", target, unwrapPathErr(err))
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", target)
	}

	if li, err := os.Lstat(link); err == nil {
		if li.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if cur, err := os.Readlink(link); err == nil && samePath(cur, target) {
				return nil
			}
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("cannot replace old link %s: %v", link, unwrapPathErr(err))
			}
		} else {
			if samePath(link, target) {
				return nil // the container path already is this folder
			}
			return fmt.Errorf("%s already exists and is a real folder; rename it, or set drive = in ffagent.conf", link)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("cannot create link %s: %v", link, unwrapPathErr(err))
	}
	return nil
}

func samePath(a, b string) bool {
	norm := func(p string) string {
		p = strings.TrimPrefix(p, `\\?\`)
		if strings.HasPrefix(strings.ToUpper(p), `UNC\`) {
			p = `\` + p[3:]
		}
		return strings.ToLower(filepath.Clean(p))
	}
	return norm(a) == norm(b)
}

func unwrapPathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// ---- SMB log-on for [credentials] -----------------------------------------

type netResource struct {
	Scope, Type, DisplayType, Usage          uint32
	LocalName, RemoteName, Comment, Provider *uint16
}

func connectShares(creds []Credential) map[string]error {
	errs := map[string]error{}
	for _, c := range creds {
		remote, err := syscall.UTF16PtrFromString(c.Share)
		if err != nil {
			errs[c.Share] = err
			continue
		}
		user, _ := syscall.UTF16PtrFromString(c.User)
		pass, _ := syscall.UTF16PtrFromString(c.Password)
		nr := netResource{Type: resourceTypeDisk, RemoteName: remote}
		r, _, _ := procWNetAddConnection2W.Call(uintptr(unsafe.Pointer(&nr)),
			uintptr(unsafe.Pointer(pass)), uintptr(unsafe.Pointer(user)), 0)
		if r != 0 && r != errorSessionCredentialConflict && r != errorAlreadyAssigned {
			errs[c.Share] = syscall.Errno(r)
		}
	}
	return errs
}

// mirrorRoot is the drive root container paths are mirrored under, e.g. C:\.
// ffmpeg resolves "/media6/x.mkv" against the drive of its working directory.
func mirrorRoot(cfg *Config) string { return cfg.Drive + `\` }

var procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")

// pauseIfOwnConsole waits for Enter when this process is the only one attached
// to its console, i.e. Windows opened a window just for it and would close it
// (with all output) the moment we exit.
func pauseIfOwnConsole() {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return // shared with a shell (or no console): output stays visible
	}
	fmt.Print("\nPress Enter to close this window...")
	_, _ = fmt.Scanln()
}
