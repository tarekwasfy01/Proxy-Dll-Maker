//go:build windows

package main

import (
	"bufio"
	"bytes"
	"debug/pe"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed proxybuilder.ico
var embeddedIcon []byte

//go:embed ProxyBuilder_Worker.exe
var embeddedWorker []byte

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_BORDER           = 0x00800000
	ES_AUTOHSCROLL      = 0x0080
	CW_USEDEFAULT       = 0x80000000
	SW_SHOW             = 5

	WM_CREATE  = 0x0001
	WM_DESTROY = 0x0002
	WM_COMMAND = 0x0111
	WM_SETFONT = 0x0030
	WM_TIMER   = 0x0113
	WM_SETICON = 0x0080

	ICON_SMALL      = 0
	ICON_BIG        = 1
	IMAGE_ICON      = 1
	LR_LOADFROMFILE = 0x00000010
	LR_DEFAULTSIZE  = 0x00000040

	ID_DLLDIR      = 101
	ID_OUTDIR      = 102
	ID_BROWSE_DLL  = 201
	ID_BROWSE_OUT  = 202
	ID_START       = 203
	ID_CANCEL      = 204
	ID_OPEN_OUTPUT = 205
	ID_OPEN_LOG    = 206
	ID_STATUS      = 301
	ID_SUMMARY     = 302

	BIF_RETURNONLYFSDIRS = 0x00000001
	BIF_NEWDIALOGSTYLE   = 0x00000040
	BIF_EDITBOX          = 0x00000010
)

const (
	peMachineAMD64 = 0x8664
)

// --- Win32 declarations ------------------------------------------------------

type wndClassEx struct {
	Size, Style              uint32
	WndProc                  uintptr
	ClsExtra, WndExtra       int32
	Instance                 uintptr
	Icon, Cursor, Background uintptr
	MenuName, ClassName      *uint16
	IconSm                   uintptr
}

type point struct{ X, Y int32 }
type msg struct {
	Hwnd           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Pt             point
	Private        uint32
}

type browseInfo struct {
	HwndOwner, PidlRoot       uintptr
	PszDisplayName, LpszTitle *uint16
	UlFlags                   uint32
	Lpfn, LParam              uintptr
	IImage                    int32
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procSetWindowTextW   = user32.NewProc("SetWindowTextW")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procEnableWindow     = user32.NewProc("EnableWindow")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")

	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
)

func wide(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}
func lowWord(v uintptr) uint16 { return uint16(v & 0xffff) }

// --- Main mode split ---------------------------------------------------------

func main() {
	if len(os.Args) >= 2 && strings.EqualFold(os.Args[1], "--worker") {
		workerMain(os.Args[2:])
		return
	}

	// CRITICAL: Win32 windows, COM apartment state and the message loop are
	// thread-affine. Previous versions did not pin this goroutine, allowing the
	// Go scheduler to move it between OS threads. That can cause apparent hangs
	// in native dialogs/message dispatch. Keep the GUI on exactly one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	guiMain()
}

// --- GUI --------------------------------------------------------------------

var gui struct {
	hwnd, hDll, hOut                  uintptr
	hStart, hCancel, hOpenOutput      uintptr
	hOpenLog, hStatus, hSummary, font uintptr
	running                           bool
	workerPID                         int
	jobDir, statusPath, cancelPath    string
	outputDir, logPath                string
	lastStatus                        string
	iconTempDir                       string
	workerExtractDir                  string
}

func guiMain() {
	procCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED on locked OS thread.
	defer procCoUninitialize.Call()

	inst, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, 32512)
	className := wide("ProxyBuilderDirectPEV3Class")
	wc := wndClassEx{
		Size:       uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:    syscall.NewCallback(windowProc),
		Instance:   inst,
		Cursor:     cursor,
		Background: 6,
		ClassName:  className,
	}
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(wide("ProxyBuilder - Direct PE FreezeFix v3"))),
		WS_OVERLAPPEDWINDOW|WS_VISIBLE,
		CW_USEDEFAULT, CW_USEDEFAULT, 900, 440,
		0, 0, inst, 0,
	)
	if hwnd == 0 {
		return
	}
	gui.hwnd = hwnd
	procShowWindow.Call(hwnd, SW_SHOW)
	procUpdateWindow.Call(hwnd)

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func createControl(class, text string, style uintptr, x, y, w, h int32, id int) uintptr {
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(wide(class))),
		uintptr(unsafe.Pointer(wide(text))),
		style|WS_CHILD|WS_VISIBLE,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		gui.hwnd, uintptr(id), 0, 0,
	)
	if hwnd != 0 && gui.font != 0 {
		procSendMessageW.Call(hwnd, WM_SETFONT, gui.font, 1)
	}
	return hwnd
}

func windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case WM_CREATE:
		gui.hwnd = hwnd
		fontHeight := int32(-17)
		gui.font, _, _ = procCreateFontW.Call(
			uintptr(uint32(fontHeight)), 0, 0, 0, 400, 0, 0, 0,
			1, 0, 0, 5, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))),
		)
		installWindowIcon(hwnd)

		createControl("STATIC", "DLL folder", 0, 20, 25, 95, 24, 0)
		gui.hDll = createControl("EDIT", "", WS_BORDER|ES_AUTOHSCROLL, 120, 20, 620, 28, ID_DLLDIR)
		createControl("BUTTON", "Browse...", 0, 750, 19, 115, 30, ID_BROWSE_DLL)

		createControl("STATIC", "Output folder", 0, 20, 65, 95, 24, 0)
		gui.hOut = createControl("EDIT", "", WS_BORDER|ES_AUTOHSCROLL, 120, 60, 620, 28, ID_OUTDIR)
		createControl("BUTTON", "Browse...", 0, 750, 59, 115, 30, ID_BROWSE_OUT)

		gui.hStart = createControl("BUTTON", "START", 0, 20, 110, 150, 38, ID_START)
		gui.hCancel = createControl("BUTTON", "CANCEL", 0, 180, 110, 150, 38, ID_CANCEL)
		gui.hOpenOutput = createControl("BUTTON", "OPEN OUTPUT", 0, 340, 110, 150, 38, ID_OPEN_OUTPUT)
		gui.hOpenLog = createControl("BUTTON", "OPEN LOG", 0, 500, 110, 150, 38, ID_OPEN_LOG)
		procEnableWindow.Call(gui.hCancel, 0)
		procEnableWindow.Call(gui.hOpenOutput, 0)
		procEnableWindow.Call(gui.hOpenLog, 0)

		createControl("STATIC", "Status", 0, 20, 175, 95, 24, 0)
		gui.hStatus = createControl("STATIC", "Ready", 0, 120, 175, 745, 26, ID_STATUS)
		createControl("STATIC", "Summary", 0, 20, 220, 95, 24, 0)
		gui.hSummary = createControl("STATIC", "No job running. Full build output is written to ProxyBuilder.log instead of the GUI.", 0, 120, 220, 745, 75, ID_SUMMARY)

		// The timer reads ONE tiny status file only. No live log stream, no
		// large EDIT control, no expensive text appends.
		procSetTimer.Call(hwnd, 1, 500, 0)
		return 0

	case WM_TIMER:
		pollStatus()
		return 0

	case WM_COMMAND:
		switch int(lowWord(wParam)) {
		case ID_BROWSE_DLL:
			if !gui.running {
				if dir := browseFolder(hwnd, "Select folder containing DLL files"); dir != "" {
					setText(gui.hDll, dir)
					if strings.TrimSpace(getText(gui.hOut)) == "" {
						setText(gui.hOut, filepath.Join(dir, "shims"))
					}
				}
			}
		case ID_BROWSE_OUT:
			if !gui.running {
				if dir := browseFolder(hwnd, "Select output folder"); dir != "" {
					setText(gui.hOut, dir)
				}
			}
		case ID_START:
			startWorker()
		case ID_CANCEL:
			cancelWorker()
		case ID_OPEN_OUTPUT:
			if gui.outputDir != "" {
				shellOpen(gui.outputDir)
			}
		case ID_OPEN_LOG:
			if gui.logPath != "" {
				shellOpen(gui.logPath)
			}
		}
		return 0

	case WM_DESTROY:
		procKillTimer.Call(hwnd, 1)
		if gui.running {
			_ = os.WriteFile(gui.cancelPath, []byte("1"), 0644)
		}
		if gui.iconTempDir != "" {
			_ = os.RemoveAll(gui.iconTempDir)
		}
		if gui.workerExtractDir != "" {
			_ = os.RemoveAll(gui.workerExtractDir)
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func startWorker() {
	if gui.running {
		return
	}
	dllDir := strings.TrimSpace(getText(gui.hDll))
	outDir := strings.TrimSpace(getText(gui.hOut))
	if dllDir == "" || outDir == "" {
		setText(gui.hStatus, "Please select both folders.")
		return
	}
	if st, err := os.Stat(dllDir); err != nil || !st.IsDir() {
		setText(gui.hStatus, "The DLL folder does not exist.")
		return
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		setText(gui.hStatus, "Cannot create output folder: "+err.Error())
		return
	}

	jobDir, err := os.MkdirTemp("", "ProxyBuilderFreezeFixJob_")
	if err != nil {
		setText(gui.hStatus, "Cannot create job state: "+err.Error())
		return
	}
	gui.jobDir = jobDir
	gui.statusPath = filepath.Join(jobDir, "status.txt")
	gui.cancelPath = filepath.Join(jobDir, "cancel.txt")
	gui.outputDir = outDir
	gui.logPath = filepath.Join(outDir, "ProxyBuilder.log")
	gui.lastStatus = ""
	_ = os.Remove(gui.cancelPath)
	_ = os.WriteFile(gui.statusPath, []byte("STARTING"), 0644)

	if len(embeddedWorker) == 0 {
		setText(gui.hStatus, "Worker could not start: embedded worker is empty")
		return
	}
	workerDir, err := os.MkdirTemp("", "ProxyBuilderWorker_")
	if err != nil {
		setText(gui.hStatus, "Worker could not start: "+err.Error())
		return
	}
	workerPath := filepath.Join(workerDir, "ProxyBuilder_Worker.exe")
	if err := os.WriteFile(workerPath, embeddedWorker, 0700); err != nil {
		_ = os.RemoveAll(workerDir)
		setText(gui.hStatus, "Worker could not start: "+err.Error())
		return
	}
	gui.workerExtractDir = workerDir

	cmd := exec.Command(
		workerPath, "--worker",
		"--dll-dir", dllDir,
		"--out-dir", outDir,
		"--status", gui.statusPath,
		"--cancel", gui.cancelPath,
		"--log", gui.logPath,
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(workerDir)
		gui.workerExtractDir = ""
		setText(gui.hStatus, "Worker could not start: "+err.Error())
		return
	}
	gui.workerPID = cmd.Process.Pid
	_ = cmd.Process.Release()

	gui.running = true
	procEnableWindow.Call(gui.hStart, 0)
	procEnableWindow.Call(gui.hCancel, 1)
	procEnableWindow.Call(gui.hOpenOutput, 1)
	procEnableWindow.Call(gui.hOpenLog, 1)
	setText(gui.hStatus, "Worker started. The GUI is idle and only polls status every 500 ms.")
	setText(gui.hSummary, "Direct-PE generation is isolated in a second process. Full details: "+gui.logPath)
}

func cancelWorker() {
	if !gui.running {
		return
	}
	_ = os.WriteFile(gui.cancelPath, []byte("1"), 0644)
	setText(gui.hStatus, "Cancellation requested. The worker will stop the current tool and exit.")
}

func pollStatus() {
	if !gui.running || gui.statusPath == "" {
		return
	}
	b, err := os.ReadFile(gui.statusPath)
	if err != nil || len(b) == 0 {
		return
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == gui.lastStatus {
		return
	}
	gui.lastStatus = s

	parts := strings.SplitN(s, "|", 5)
	switch parts[0] {
	case "STARTING":
		setText(gui.hStatus, "Starting worker...")
	case "READY":
		if len(parts) >= 2 {
			setText(gui.hStatus, "Build tools ready. DLLs found: "+parts[1])
		}
	case "RUN":
		if len(parts) >= 4 {
			setText(gui.hStatus, parts[1]+" / "+parts[2]+"   "+parts[3])
		}
	case "DONE":
		gui.running = false
		procEnableWindow.Call(gui.hStart, 1)
		procEnableWindow.Call(gui.hCancel, 0)
		if len(parts) >= 4 {
			setText(gui.hStatus, "Finished")
			setText(gui.hSummary, "Created: "+parts[1]+"   Failed: "+parts[2]+"   Skipped: "+parts[3]+". Full log: "+gui.logPath)
		}
		cleanupJobState()
	case "CANCELLED":
		gui.running = false
		procEnableWindow.Call(gui.hStart, 1)
		procEnableWindow.Call(gui.hCancel, 0)
		setText(gui.hStatus, "Cancelled")
		setText(gui.hSummary, "The build was cancelled. Partial results and the log remain in the output folder.")
		cleanupJobState()
	case "ERROR":
		gui.running = false
		procEnableWindow.Call(gui.hStart, 1)
		procEnableWindow.Call(gui.hCancel, 0)
		msg := "Worker error"
		if len(parts) >= 2 {
			msg = parts[1]
		}
		setText(gui.hStatus, "Error")
		setText(gui.hSummary, msg+". See: "+gui.logPath)
		cleanupJobState()
	}
}

func cleanupJobState() {
	if gui.jobDir != "" {
		_ = os.RemoveAll(gui.jobDir)
	}
	if gui.workerExtractDir != "" {
		_ = os.RemoveAll(gui.workerExtractDir)
	}
	gui.jobDir, gui.statusPath, gui.cancelPath = "", "", ""
	gui.workerExtractDir = ""
}

func installWindowIcon(hwnd uintptr) {
	if len(embeddedIcon) == 0 {
		return
	}
	dir, err := os.MkdirTemp("", "ProxyBuilderIcon_")
	if err != nil {
		return
	}
	path := filepath.Join(dir, "proxybuilder.ico")
	if err := os.WriteFile(path, embeddedIcon, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return
	}
	gui.iconTempDir = dir
	if hIcon, _, _ := procLoadImageW.Call(
		0, uintptr(unsafe.Pointer(wide(path))), IMAGE_ICON, 0, 0,
		LR_LOADFROMFILE|LR_DEFAULTSIZE,
	); hIcon != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hIcon)
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hIcon)
	}
}

func browseFolder(owner uintptr, title string) string {
	var display [260]uint16
	bi := browseInfo{
		HwndOwner:      owner,
		PszDisplayName: &display[0],
		LpszTitle:      wide(title),
		UlFlags:        BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE | BIF_EDITBOX,
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)
	var path [32768]uint16
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(path[:])
}

func setText(hwnd uintptr, text string) {
	if hwnd != 0 {
		procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(wide(text))))
	}
}
func getText(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	buf := make([]uint16, 32768)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}
func shellOpen(path string) {
	procShellExecuteW.Call(gui.hwnd, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(path))), 0, 0, SW_SHOW)
}

// --- Worker -----------------------------------------------------------------

type workerConfig struct {
	DLLDir, OutDir, StatusPath, CancelPath, LogPath string
}

type workerLogger struct {
	w *bufio.Writer
}

func (l *workerLogger) line(s string) {
	if l.w != nil {
		_, _ = l.w.WriteString(s + "\r\n")
		_ = l.w.Flush() // flush Go buffer only; do NOT force a physical disk Sync per line.
	}
}

func workerMain(args []string) {
	cfg := workerConfig{
		DLLDir:     argValue(args, "--dll-dir"),
		OutDir:     argValue(args, "--out-dir"),
		StatusPath: argValue(args, "--status"),
		CancelPath: argValue(args, "--cancel"),
		LogPath:    argValue(args, "--log"),
	}
	if cfg.DLLDir == "" || cfg.OutDir == "" || cfg.StatusPath == "" || cfg.CancelPath == "" || cfg.LogPath == "" {
		return
	}
	_ = os.MkdirAll(cfg.OutDir, 0755)
	logFile, err := os.Create(cfg.LogPath)
	if err != nil {
		writeStatus(cfg.StatusPath, "ERROR|Cannot create log file")
		return
	}
	logger := &workerLogger{w: bufio.NewWriterSize(logFile, 64*1024)}
	defer logFile.Close()

	logger.line("ProxyBuilder Freeze Fix")
	logger.line("Architecture: GUI process -> isolated worker process -> one MSVC tool at a time")
	logger.line("Live compiler output is intentionally NOT sent to the GUI.")

	toolset, err := detectMSVCToolset(cfg.CancelPath, logger)
	if err != nil {
		logger.line("ERROR: " + err.Error())
		writeStatus(cfg.StatusPath, "ERROR|"+statusEscape(err.Error()))
		return
	}

	all, err := filepath.Glob(filepath.Join(cfg.DLLDir, "*.dll"))
	if err != nil {
		logger.line("ERROR: " + err.Error())
		writeStatus(cfg.StatusPath, "ERROR|"+statusEscape(err.Error()))
		return
	}
	sort.Strings(all)
	files := make([]string, 0, len(all))
	for _, p := range all {
		n := strings.ToLower(filepath.Base(p))
		if strings.HasSuffix(n, "_proxy.dll") || strings.HasSuffix(n, "_o.dll") || n == "universal_proxy.dll" {
			logger.line("SKIP generated/support DLL: " + filepath.Base(p))
			continue
		}
		files = append(files, p)
	}
	writeStatus(cfg.StatusPath, fmt.Sprintf("READY|%d", len(files)))
	logger.line(fmt.Sprintf("Input DLLs: %d", len(files)))

	batchDir, err := os.MkdirTemp("", "ProxyBuilderBatch_")
	if err != nil {
		logger.line("ERROR: " + err.Error())
		writeStatus(cfg.StatusPath, "ERROR|"+statusEscape(err.Error()))
		return
	}
	defer os.RemoveAll(batchDir)

	genericObj := filepath.Join(batchDir, "generic_proxy.obj")
	if err := compileGenericObject(toolset, genericObj, cfg.CancelPath); err != nil {
		logger.line("ERROR compiling reusable proxy stub: " + err.Error())
		writeStatus(cfg.StatusPath, "ERROR|"+statusEscape(err.Error()))
		return
	}
	logger.line("Reusable proxy stub compiled once for the complete folder.")

	success, failed, skipped := 0, 0, 0
	for i, dll := range files {
		if isCancelled(cfg.CancelPath) {
			logger.line("CANCELLED")
			writeStatus(cfg.StatusPath, "CANCELLED")
			return
		}
		name := filepath.Base(dll)
		writeStatus(cfg.StatusPath, fmt.Sprintf("RUN|%d|%d|%s", i+1, len(files), statusEscape(name)))
		logger.line(fmt.Sprintf("[%d/%d] %s", i+1, len(files), name))

		machine, err := peMachine(dll)
		if err != nil {
			skipped++
			logger.line("  SKIP invalid PE/DLL: " + err.Error())
			continue
		}
		if machine != peMachineAMD64 {
			skipped++
			logger.line(fmt.Sprintf("  SKIP unsupported architecture 0x%04X (this builder creates x64 proxies)", machine))
			continue
		}

		base := strings.TrimSuffix(name, filepath.Ext(name))
		out := filepath.Join(cfg.OutDir, base+"_proxy.dll")
		forward := base + "_o.dll"
		created, err := buildOneProxy(toolset, genericObj, dll, out, forward, cfg.CancelPath)
		if errors.Is(err, errCancelled) {
			logger.line("  CANCELLED")
			writeStatus(cfg.StatusPath, "CANCELLED")
			return
		}
		if err != nil {
			failed++
			logger.line("  ERROR: " + err.Error())
			_ = os.Remove(out)
			continue
		}
		if !created {
			skipped++
			logger.line("  SKIP no named exports")
			continue
		}
		success++
		logger.line("  OK -> " + out)
	}

	logger.line("")
	logger.line(fmt.Sprintf("DONE. Created=%d Failed=%d Skipped=%d", success, failed, skipped))
	writeStatus(cfg.StatusPath, fmt.Sprintf("DONE|%d|%d|%d", success, failed, skipped))
}

func argValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if strings.EqualFold(args[i], name) {
			return args[i+1]
		}
	}
	return ""
}
func writeStatus(path, status string) {
	// tiny fixed-size file; no GUI log traffic.
	_ = os.WriteFile(path, []byte(status), 0644)
}
func statusEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
func isCancelled(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// --- Integrated batch builder ------------------------------------------------

type Export struct {
	Ordinal int
	Name    string
}
type msvcToolset struct {
	Cl, Link string
	Include  []string
	Lib      []string
}

var errCancelled = errors.New("cancelled")

func detectMSVCToolset(cancelPath string, logger *workerLogger) (msvcToolset, error) {
	var t msvcToolset
	if isCancelled(cancelPath) {
		return t, errCancelled
	}

	vswhere := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
	if _, err := os.Stat(vswhere); err != nil {
		vswhere = filepath.Join(os.Getenv("ProgramFiles"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
	}
	if _, err := os.Stat(vswhere); err != nil {
		return t, fmt.Errorf("vswhere.exe not found")
	}

	out, err := runExternal(cancelPath, 20*time.Second, nil, vswhere,
		"-latest", "-products", "*",
		"-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
		"-property", "installationPath")
	if err != nil {
		return t, fmt.Errorf("vswhere failed: %w", err)
	}
	vs := strings.TrimSpace(string(out))
	if vs == "" {
		return t, fmt.Errorf("Visual Studio C++ Build Tools not found")
	}

	versions, _ := filepath.Glob(filepath.Join(vs, "VC", "Tools", "MSVC", "*"))
	versions = existingDirs(versions)
	if len(versions) == 0 {
		return t, fmt.Errorf("MSVC toolset not found")
	}
	sort.Strings(versions)
	msvc := versions[len(versions)-1]
	binDir := filepath.Join(msvc, "bin", "Hostx64", "x64")
	t.Cl = filepath.Join(binDir, "cl.exe")
	t.Link = filepath.Join(binDir, "link.exe")
	for _, p := range []string{t.Cl, t.Link} {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return t, fmt.Errorf("missing MSVC tool: %s", p)
		}
	}

	sdkRoot := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Windows Kits", "10")
	sdkDirs, _ := filepath.Glob(filepath.Join(sdkRoot, "Include", "*"))
	sdkDirs = existingDirs(sdkDirs)
	if len(sdkDirs) == 0 {
		return t, fmt.Errorf("Windows SDK not found")
	}
	sort.Strings(sdkDirs)
	sdk := filepath.Base(sdkDirs[len(sdkDirs)-1])

	t.Include = []string{
		filepath.Join(msvc, "include"),
		filepath.Join(sdkRoot, "Include", sdk, "ucrt"),
		filepath.Join(sdkRoot, "Include", sdk, "shared"),
		filepath.Join(sdkRoot, "Include", sdk, "um"),
	}
	t.Lib = []string{
		filepath.Join(msvc, "lib", "x64"),
		filepath.Join(sdkRoot, "Lib", sdk, "ucrt", "x64"),
		filepath.Join(sdkRoot, "Lib", sdk, "um", "x64"),
	}
	logger.line("MSVC detected once: " + msvc)
	logger.line("Windows SDK detected once: " + sdk)
	return t, nil
}

func existingDirs(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func toolEnv(t msvcToolset) []string {
	env := os.Environ()
	env = append(env, "INCLUDE="+strings.Join(t.Include, ";"))
	env = append(env, "LIB="+strings.Join(t.Lib, ";"))
	return env
}

func compileGenericObject(t msvcToolset, outObj, cancelPath string) error {
	dir := filepath.Dir(outObj)
	cpp := filepath.Join(dir, "generic_proxy.cpp")
	source := `#define WIN32_LEAN_AND_MEAN
#include <windows.h>
static volatile char kHomeClusterSafeProxyMarker[] = "HC_SAFE_FORWARD_V8";
BOOL WINAPI DllMain(HINSTANCE, DWORD, LPVOID) {
    volatile char marker = kHomeClusterSafeProxyMarker[0];
    (void)marker;
    return TRUE;
}
`
	if err := os.WriteFile(cpp, []byte(source), 0644); err != nil {
		return err
	}
	out, err := runExternal(cancelPath, 60*time.Second, toolEnv(t), t.Cl,
		"/nologo", "/c", "/O2", "/MD", "/EHsc", "/Fo"+outObj, cpp)
	if err != nil {
		return fmt.Errorf("cl generic stub: %v: %s", err, trimOutput(out))
	}
	return nil
}

func buildOneProxy(t msvcToolset, genericObj, dll, out, forwardModule, cancelPath string) (bool, error) {
	exports, err := readNamedExportsPE(dll)
	if err != nil {
		return false, err
	}
	if len(exports) == 0 {
		return false, nil
	}

	tempDir, err := os.MkdirTemp("", "PBOne_")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tempDir)

	wrap := map[string]bool{}
	if strings.EqualFold(filepath.Base(dll), "abovelockapphost.dll") {
		for _, e := range exports {
			if e.Name == "DllCanUnloadNow" {
				wrap[e.Name] = true
			}
		}
	}

	obj := genericObj
	if len(wrap) != 0 {
		obj = filepath.Join(tempDir, "proxy.obj")
		cpp := filepath.Join(tempDir, "proxy.cpp")
		if err := os.WriteFile(cpp, []byte(wrapperSource(dll, forwardModule, exports, wrap)), 0644); err != nil {
			return false, err
		}
		toolOut, err := runExternal(cancelPath, 60*time.Second, toolEnv(t), t.Cl,
			"/nologo", "/c", "/O2", "/MD", "/EHsc", "/Fo"+obj, cpp)
		if err != nil {
			return false, fmt.Errorf("compile failed: %v: %s", err, trimOutput(toolOut))
		}
	}

	def := filepath.Join(tempDir, "proxy.def")
	if err := os.WriteFile(def, []byte(makeDEF(dll, forwardModule, exports, wrap)), 0644); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return false, err
	}

	implib := filepath.Join(tempDir, "proxy.lib")
	toolOut, err := runExternal(cancelPath, 90*time.Second, toolEnv(t), t.Link,
		"/nologo", "/DLL", "/MACHINE:X64", "/INCREMENTAL:NO", "/OUT:"+out, "/DEF:"+def, "/IMPLIB:"+implib, obj, "kernel32.lib")
	if err != nil {
		return false, fmt.Errorf("link failed: %v: %s", err, trimOutput(toolOut))
	}

	got, err := readNamedExportsPE(out)
	if err != nil {
		return false, fmt.Errorf("verify exports: %w", err)
	}
	if len(got) != len(exports) {
		return false, fmt.Errorf("verify export count mismatch: source=%d proxy=%d", len(exports), len(got))
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		return false, err
	}
	forwardBase := strings.TrimSuffix(filepath.Base(forwardModule), filepath.Ext(forwardModule))
	if !bytes.Contains(bytes.ToLower(bin), bytes.ToLower([]byte(forwardBase))) {
		return false, fmt.Errorf("verify failed: forward module %q is not embedded", forwardModule)
	}
	if !bytes.Contains(bin, []byte("HC_SAFE_FORWARD_V8")) {
		return false, fmt.Errorf("verify failed: safe marker missing")
	}
	return true, nil
}

func wrapperSource(dll, forwardModule string, exports []Export, wrap map[string]bool) string {
	var c strings.Builder
	c.WriteString(`#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>
typedef uintptr_t (__cdecl *HCUniversalCallFn)(const char*, const char*, int, const char* const*);
static HMODULE g_core = NULL;
static HMODULE g_orig = NULL;
static volatile char kHomeClusterSafeProxyMarker[] = "HC_SAFE_FORWARD_V8";
static HCUniversalCallFn getHC(void) {
    if (!g_core) g_core = LoadLibraryW(L"universal_proxy.dll");
    if (!g_core) return NULL;
    return (HCUniversalCallFn)GetProcAddress(g_core, "HC_UniversalCall");
}
static FARPROC getOrig(const char* name) {
    if (!g_orig) g_orig = LoadLibraryW(L"` + escapeC(filepath.Base(forwardModule)) + `");
    if (!g_orig) return NULL;
    return GetProcAddress(g_orig, name);
}
BOOL WINAPI DllMain(HINSTANCE, DWORD, LPVOID) {
    volatile char marker = kHomeClusterSafeProxyMarker[0];
    (void)marker; return TRUE;
}
`)
	for _, e := range exports {
		if !wrap[e.Name] {
			continue
		}
		id := cIdent(e.Name)
		c.WriteString("\nextern \"C\" HRESULT WINAPI HCW_" + id + "(void) {\n")
		c.WriteString("    HCUniversalCallFn hc = getHC();\n")
		c.WriteString("    if (hc) { uintptr_t r = hc(\"" + escapeC(filepath.Base(dll)) + "\", \"" + escapeC(e.Name) + "\", 0, NULL); if (r != 0) return (HRESULT)r; }\n")
		c.WriteString("    typedef HRESULT (WINAPI *OrigFn)(void);\n")
		c.WriteString("    OrigFn fn = (OrigFn)getOrig(\"" + escapeC(e.Name) + "\");\n")
		c.WriteString("    if (!fn) return E_FAIL; return fn();\n}\n")
	}
	return c.String()
}

func makeDEF(dll, forwardModule string, exports []Export, wrap map[string]bool) string {
	base := strings.TrimSuffix(filepath.Base(dll), filepath.Ext(dll))
	forwardBase := strings.TrimSuffix(filepath.Base(forwardModule), filepath.Ext(forwardModule))
	var d strings.Builder
	d.WriteString("LIBRARY " + base + "\nEXPORTS\n")
	for _, e := range exports {
		lhs := defQuote(e.Name)
		if wrap[e.Name] {
			d.WriteString(fmt.Sprintf("    %s=HCW_%s @%d\n", lhs, cIdent(e.Name), e.Ordinal))
		} else {
			d.WriteString(fmt.Sprintf("    %s=%s.#%d @%d\n", lhs, forwardBase, e.Ordinal, e.Ordinal))
		}
	}
	return d.String()
}

func runExternal(cancelPath string, timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	if isCancelled(cancelPath) {
		return nil, errCancelled
	}
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return output.Bytes(), err
	}

	// This one waiter goroutine exists only inside the isolated worker process.
	// It is never part of the GUI message loop and there is still only one
	// external tool process at a time.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return output.Bytes(), err
		case <-ticker.C:
			if isCancelled(cancelPath) {
				_ = cmd.Process.Kill()
				<-done
				return output.Bytes(), errCancelled
			}
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return output.Bytes(), fmt.Errorf("tool timeout after %s", timeout)
		}
	}
}

func trimOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 4000 {
		s = s[:4000] + " ..."
	}
	return s
}

// Native PE export reader: replaces two dumpbin.exe launches per DLL.
func readNamedExportsPE(path string) ([]Export, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var exportRVA, exportSize, sizeHeaders uint32
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		exportRVA = oh.DataDirectory[0].VirtualAddress
		exportSize = oh.DataDirectory[0].Size
		sizeHeaders = oh.SizeOfHeaders
	case *pe.OptionalHeader32:
		exportRVA = oh.DataDirectory[0].VirtualAddress
		exportSize = oh.DataDirectory[0].Size
		sizeHeaders = oh.SizeOfHeaders
	default:
		return nil, fmt.Errorf("unsupported PE optional header")
	}
	if exportRVA == 0 || exportSize == 0 {
		return nil, nil
	}

	rvaOffset := func(rva uint32) (uint32, error) {
		if rva < sizeHeaders && int(rva) < len(data) {
			return rva, nil
		}
		for _, s := range f.Sections {
			start := s.VirtualAddress
			size := s.VirtualSize
			if s.Size > size {
				size = s.Size
			}
			if rva >= start && rva < start+size {
				off := s.Offset + (rva - start)
				if int(off) >= len(data) {
					break
				}
				return off, nil
			}
		}
		return 0, fmt.Errorf("RVA 0x%X is outside file sections", rva)
	}
	exportOff, err := rvaOffset(exportRVA)
	if err != nil {
		return nil, err
	}
	if int(exportOff)+40 > len(data) {
		return nil, fmt.Errorf("truncated export directory")
	}

	u32 := func(off uint32) (uint32, error) {
		if int(off)+4 > len(data) {
			return 0, ioBoundsError(off, 4)
		}
		return binary.LittleEndian.Uint32(data[off : off+4]), nil
	}
	u16 := func(off uint32) (uint16, error) {
		if int(off)+2 > len(data) {
			return 0, ioBoundsError(off, 2)
		}
		return binary.LittleEndian.Uint16(data[off : off+2]), nil
	}
	baseOrd, _ := u32(exportOff + 16)
	numberNames, _ := u32(exportOff + 24)
	namesRVA, _ := u32(exportOff + 32)
	ordsRVA, _ := u32(exportOff + 36)
	if numberNames > 200000 {
		return nil, fmt.Errorf("unreasonable export name count: %d", numberNames)
	}
	if numberNames == 0 {
		return nil, nil
	}
	namesOff, err := rvaOffset(namesRVA)
	if err != nil {
		return nil, err
	}
	ordsOff, err := rvaOffset(ordsRVA)
	if err != nil {
		return nil, err
	}

	result := make([]Export, 0, numberNames)
	for i := uint32(0); i < numberNames; i++ {
		nameRVA, err := u32(namesOff + i*4)
		if err != nil {
			return nil, err
		}
		nameOff, err := rvaOffset(nameRVA)
		if err != nil {
			return nil, err
		}
		ordIndex, err := u16(ordsOff + i*2)
		if err != nil {
			return nil, err
		}
		name := readCString(data, nameOff, 32768)
		if name == "" {
			continue
		}
		result = append(result, Export{Ordinal: int(baseOrd) + int(ordIndex), Name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ordinal < result[j].Ordinal })
	return result, nil
}

func peMachine(path string) (uint16, error) {
	f, err := pe.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.FileHeader.Machine, nil
}
func readCString(data []byte, off uint32, max int) string {
	if int(off) >= len(data) {
		return ""
	}
	end := int(off)
	limit := end + max
	if limit > len(data) {
		limit = len(data)
	}
	for end < limit && data[end] != 0 {
		end++
	}
	return string(data[int(off):end])
}
func ioBoundsError(off uint32, n int) error {
	return fmt.Errorf("PE read out of bounds at 0x%X (+%d)", off, n)
}

func defQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func cIdent(s string) string {
	var b strings.Builder
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteString(fmt.Sprintf("_%02X", r))
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
func escapeC(s string) string {
	s = strings.ReplaceAll(s, `\\`, `\\\\`)
	s = strings.ReplaceAll(s, `"`, `\\"`)
	return s
}

// Keep strconv linked in source builds for easy diagnostic expansion.
var _ = strconv.Itoa
