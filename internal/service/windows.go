//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// oldWindowsServiceName is the Windows Service that earlier versions
	// registered. It could not start: the binary never answered the
	// service manager, and binPath pointed at a file install never wrote.
	oldWindowsServiceName = "QuantifaiSync"
	// legacyWindowsServiceName is the old service name used by ai-ops-shipper.
	legacyWindowsServiceName = "AIOpsShipper"
)

// Windows implements the Installer interface for Windows. It registers a
// per-user Task Scheduler logon task (see taskTemplate) that runs the
// current executable as the installing user. No elevation is needed: a
// standard user registered this task for themselves on windows-latest.
// Install must run as the user the agent should run as, because the task
// and the Credential Manager key belong to the account that runs install.
type Windows struct {
	taskName string
	binPath  string
	args     string
	user     string
}

// NewWindows returns a Windows installer for the current executable and user.
func NewWindows() *Windows {
	binPath, err := os.Executable()
	if err != nil {
		binPath = "quantifai-sync.exe"
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return &Windows{taskName: windowsTaskName, binPath: binPath, args: taskArguments, user: name}
}

// ConfigPath returns an empty string: the task lives in Task Scheduler,
// not in a file this program manages.
func (w *Windows) ConfigPath() string {
	return ""
}

// Install removes the service earlier versions registered, registers the
// logon task, and starts it now.
func (w *Windows) Install() error {
	if w.user == "" {
		return fmt.Errorf("service: could not determine the current user")
	}
	// Elevating with another administrator's credentials runs install as
	// that administrator, inside the signed-in user's session. The task and
	// the key would then belong to the administrator instead.
	if signedIn := sessionUser(); signedIn != "" && otherAccount(signedIn) {
		return fmt.Errorf("service: install is running as %s, but %s is signed in to this session; "+
			"the agent runs as the account that installs it, so run install as %s from a prompt that is not elevated", w.user, signedIn, signedIn)
	}
	removeService(oldWindowsServiceName)
	if err := w.register(); err != nil {
		return err
	}
	fmt.Printf("registered logon task %s for %s\n", w.taskName, w.user)
	// StopExisting makes this replace an instance that is still running.
	if out, err := exec.Command("schtasks", "/run", "/tn", w.taskName).CombinedOutput(); err != nil {
		return fmt.Errorf("service: schtasks /run: %s: %w", out, err)
	}
	return nil
}

// register writes the task definition and creates (or replaces) the task.
func (w *Windows) register() error {
	f, err := os.CreateTemp("", "quantifai-sync-task-*.xml")
	if err != nil {
		return fmt.Errorf("service: create task file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	_, err = f.Write(utf16LE(GenerateTaskXML(w.user, w.binPath, w.args)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("service: write task file: %w", err)
	}
	out, err := exec.Command("schtasks", "/create", "/tn", w.taskName, "/xml", filepath.Clean(path), "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("service: schtasks /create: %s: %w", out, err)
	}
	return nil
}

// Uninstall stops the agent and deletes the logon task, and removes the
// service earlier versions registered.
func (w *Windows) Uninstall() error {
	exec.Command("schtasks", "/end", "/tn", w.taskName).CombinedOutput() // best-effort
	out, err := exec.Command("schtasks", "/delete", "/tn", w.taskName, "/f").CombinedOutput()
	removeService(oldWindowsServiceName)
	if err != nil {
		return fmt.Errorf("service: schtasks /delete: %s: %w", out, err)
	}
	return nil
}

// MigrateFromOld detects the old AIOpsShipper Windows Service, stops it,
// and removes it.  Returns nil if no old service was found.
func (w *Windows) MigrateFromOld() error {
	// Check if old service exists (best-effort query)
	if _, err := exec.Command("sc", "query", legacyWindowsServiceName).CombinedOutput(); err != nil {
		return nil // service not found, nothing to migrate
	}

	// Stop the old service (best-effort)
	exec.Command("sc", "stop", legacyWindowsServiceName).CombinedOutput()

	// Delete the old service
	if _, err := exec.Command("sc", "delete", legacyWindowsServiceName).CombinedOutput(); err != nil {
		return fmt.Errorf("service: sc delete legacy service: %w", err)
	}

	return nil
}

// sessionUser returns DOMAIN\user signed in to this process's session, or
// "" if it cannot be read.
func sessionUser() string {
	query := windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")
	if query.Find() != nil {
		return ""
	}
	const (
		currentServer  = 0          // WTS_CURRENT_SERVER_HANDLE
		currentSession = 0xFFFFFFFF // WTS_CURRENT_SESSION
		wtsUserName    = 5
		wtsDomainName  = 7
	)
	get := func(class uintptr) string {
		var buf *uint16
		var n uint32
		r, _, _ := query.Call(currentServer, currentSession, class, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
		if r == 0 || buf == nil {
			return ""
		}
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
		return windows.UTF16PtrToString(buf)
	}
	name := get(wtsUserName)
	if name == "" {
		return ""
	}
	return get(wtsDomainName) + `\` + name
}

// otherAccount reports whether name is a different account from the one
// this process runs as. It compares SIDs, since the same account can be
// spelled differently (domain, Microsoft or Entra ID accounts), and says
// no when either account cannot be looked up, so a lookup failure never
// blocks an install.
func otherAccount(name string) bool {
	cur, err := user.Current()
	if err != nil {
		return false
	}
	if strings.EqualFold(name, cur.Username) {
		return false
	}
	other, err := user.Lookup(name)
	if err != nil {
		return false
	}
	return other.Uid != cur.Uid
}

// removeService stops and deletes a Windows Service if it exists.
func removeService(name string) {
	if _, err := exec.Command("sc", "query", name).CombinedOutput(); err != nil {
		return
	}
	exec.Command("sc", "stop", name).CombinedOutput()
	exec.Command("sc", "delete", name).CombinedOutput()
}

// utf16LE encodes s as UTF-16 little-endian with a byte-order mark, the
// encoding the task XML declares.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(units))
	b[0], b[1] = 0xFF, 0xFE
	for i, u := range units {
		b[2+2*i] = byte(u)
		b[3+2*i] = byte(u >> 8)
	}
	return b
}

func init() {
	// Register the Windows installer in the factory so that
	// NewInstaller("windows") works when compiled on Windows.
	newWindowsInstaller = func() Installer {
		return NewWindows()
	}
}
