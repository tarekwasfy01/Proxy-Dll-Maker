//go:build windows

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

//go:embed proxybuilder.exe
var embeddedProxyBuilder []byte

//go:embed proxybuilder.ico
var embeddedIcon []byte

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_BORDER           = 0x00800000
	WS_VSCROLL          = 0x00200000
	ES_AUTOHSCROLL      = 0x0080
	ES_MULTILINE        = 0x0004
	ES_AUTOVSCROLL      = 0x0040
	ES_READONLY         = 0x0800
	CW_USEDEFAULT       = 0x80000000
	SW_SHOW             = 5
	WM_CREATE           = 0x0001
	WM_DESTROY          = 0x0002
	WM_COMMAND          = 0x0111
	WM_SETFONT          = 0x0030
	WM_TIMER            = 0x0113
	WM_SETICON          = 0x0080
	ICON_SMALL          = 0
	ICON_BIG            = 1
	IMAGE_ICON          = 1
	LR_LOADFROMFILE     = 0x00000010
	LR_DEFAULTSIZE      = 0x00000040
	WM_APP_DONE         = 0x8002

	EM_SETSEL      = 0x00B1
	EM_REPLACESEL  = 0x00C2
	EM_SCROLLCARET = 0x00B7

	ID_DLLDIR     = 101
	ID_OUTDIR     = 102
	ID_BROWSE_DLL = 201
	ID_BROWSE_OUT = 202
	ID_START      = 203
	ID_LOG        = 301

	BIF_RETURNONLYFSDIRS = 0x00000001
	BIF_NEWDIALOGSTYLE   = 0x00000040
	BIF_USENEWUI         = BIF_NEWDIALOGSTYLE
)

type wndClassEx struct {
	Size, Style  uint32
	WndProc      uintptr
	ClsExtra     int32
	WndExtra     int32
	Instance     uintptr
	Icon, Cursor uintptr
	Background   uintptr
	MenuName     *uint16
	ClassName    *uint16
	IconSm       uintptr
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
	HwndOwner      uintptr
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
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
	procPostMessageW     = user32.NewProc("PostMessageW")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateFontW      = gdi32.NewProc("CreateFontW")

	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
)

var ui struct {
	hwnd     uintptr
	hDllDir  uintptr
	hOutDir  uintptr
	hStart   uintptr
	hLog     uintptr
	font     uintptr
	icon     uintptr
	iconPath string
	running  atomic.Bool

	logMu      sync.Mutex
	logPending strings.Builder
}

func wstr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func loWord(v uintptr) uint16 { return uint16(v & 0xffff) }

func main() {
	procCoInitializeEx.Call(0, 2)
	defer procCoUninitialize.Call()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, 32512)

	className := wstr("ProxyBuilderOneFileGUIClass")
	wc := wndClassEx{
		Size:       uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:    syscall.NewCallback(windowProc),
		Instance:   hInst,
		Cursor:     cursor,
		Background: 6,
		ClassName:  className,
	}
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		fmt.Fprintln(os.Stderr, "RegisterClassExW:", e)
		return
	}

	title := wstr("ProxyBuilder OneFile")
	hwnd, _, e := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		WS_OVERLAPPEDWINDOW|WS_VISIBLE,
		CW_USEDEFAULT, CW_USEDEFAULT, 850, 620,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		fmt.Fprintln(os.Stderr, "CreateWindowExW:", e)
		return
	}
	ui.hwnd = hwnd
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
		uintptr(unsafe.Pointer(wstr(class))),
		uintptr(unsafe.Pointer(wstr(text))),
		style|WS_CHILD|WS_VISIBLE,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		ui.hwnd, uintptr(id), 0, 0,
	)
	if hwnd != 0 && ui.font != 0 {
		procSendMessageW.Call(hwnd, WM_SETFONT, ui.font, 1)
	}
	return hwnd
}

func windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case WM_CREATE:
		ui.hwnd = hwnd
		if iconPath, err := extractEmbeddedIcon(); err == nil {
			ui.iconPath = iconPath
			if hIcon, _, _ := procLoadImageW.Call(
				0,
				uintptr(unsafe.Pointer(wstr(iconPath))),
				IMAGE_ICON,
				0, 0,
				LR_LOADFROMFILE|LR_DEFAULTSIZE,
			); hIcon != 0 {
				ui.icon = hIcon
				procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hIcon)
				procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hIcon)
			}
		}
		fontHeight := int32(-16)
		ui.font, _, _ = procCreateFontW.Call(
			uintptr(uint32(fontHeight)), 0, 0, 0, 400, 0, 0, 0,
			1, 0, 0, 5, 0, uintptr(unsafe.Pointer(wstr("Segoe UI"))),
		)

		createControl("STATIC", "DLL-Ordner:", 0, 18, 20, 100, 24, 0)
		ui.hDllDir = createControl("EDIT", "", WS_BORDER|ES_AUTOHSCROLL, 120, 16, 590, 26, ID_DLLDIR)
		createControl("BUTTON", "Durchsuchen", 0, 720, 15, 105, 28, ID_BROWSE_DLL)

		createControl("STATIC", "Ausgabeordner:", 0, 18, 58, 100, 24, 0)
		ui.hOutDir = createControl("EDIT", "", WS_BORDER|ES_AUTOHSCROLL, 120, 54, 590, 26, ID_OUTDIR)
		createControl("BUTTON", "Durchsuchen", 0, 720, 53, 105, 28, ID_BROWSE_OUT)

		ui.hStart = createControl("BUTTON", "START", 0, 18, 100, 160, 36, ID_START)
		createControl("STATIC", "Ausgabe / Log:", 0, 18, 150, 120, 24, 0)
		ui.hLog = createControl("EDIT", "", WS_BORDER|WS_VSCROLL|ES_MULTILINE|ES_AUTOVSCROLL|ES_READONLY, 18, 176, 807, 385, ID_LOG)

		// Flush pending log lines in small batches. This keeps the UI responsive
		// even when hundreds/thousands of DLLs are processed.
		procSetTimer.Call(hwnd, 1, 120, 0)
		appendLog("ProxyBuilder OneFile bereit.")
		appendLog("Wähle DLL-Ordner und Ausgabeordner.")
		return 0

	case WM_TIMER:
		flushPendingLog()
		return 0

	case WM_COMMAND:
		switch int(loWord(wParam)) {
		case ID_BROWSE_DLL:
			if dir := browseFolder(hwnd, "Ordner mit DLL-Dateien auswählen"); dir != "" {
				setText(ui.hDllDir, dir)
				if strings.TrimSpace(getText(ui.hOutDir)) == "" {
					setText(ui.hOutDir, filepath.Join(dir, "shims"))
				}
			}
		case ID_BROWSE_OUT:
			if dir := browseFolder(hwnd, "Ausgabeordner auswählen"); dir != "" {
				setText(ui.hOutDir, dir)
			}
		case ID_START:
			startBuild()
		}
		return 0

	case WM_APP_DONE:
		flushPendingLog()
		ui.running.Store(false)
		procEnableWindow.Call(ui.hStart, 1)
		return 0

	case WM_DESTROY:
		procKillTimer.Call(hwnd, 1)
		if ui.iconPath != "" {
			_ = os.Remove(ui.iconPath)
			_ = os.Remove(filepath.Dir(ui.iconPath))
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func browseFolder(owner uintptr, title string) string {
	var display [260]uint16
	bi := browseInfo{
		HwndOwner:      owner,
		PszDisplayName: &display[0],
		LpszTitle:      wstr(title),
		UlFlags:        BIF_RETURNONLYFSDIRS | BIF_USENEWUI,
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

func startBuild() {
	if !ui.running.CompareAndSwap(false, true) {
		return
	}

	dllDir := strings.TrimSpace(getText(ui.hDllDir))
	outDir := strings.TrimSpace(getText(ui.hOutDir))
	if dllDir == "" || outDir == "" {
		ui.running.Store(false)
		appendLog("FEHLER: Bitte DLL-Ordner und Ausgabeordner auswählen.")
		return
	}
	st, err := os.Stat(dllDir)
	if err != nil || !st.IsDir() {
		ui.running.Store(false)
		appendLog("FEHLER: DLL-Ordner existiert nicht.")
		return
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		ui.running.Store(false)
		appendLog("FEHLER: Ausgabeordner kann nicht erstellt werden: " + err.Error())
		return
	}

	procEnableWindow.Call(ui.hStart, 0)

	// Everything from here runs outside the UI thread.
	go func() {
		builder, cleanup, err := extractEmbeddedProxyBuilder()
		if err != nil {
			appendLog("FEHLER: eingebetteter ProxyBuilder konnte nicht gestartet werden: " + err.Error())
			procPostMessageW.Call(ui.hwnd, WM_APP_DONE, 0, 0)
			return
		}
		defer cleanup()

		files, err := filepath.Glob(filepath.Join(dllDir, "*.dll"))
		if err != nil {
			appendLog("FEHLER beim Lesen des DLL-Ordners: " + err.Error())
			procPostMessageW.Call(ui.hwnd, WM_APP_DONE, 0, 0)
			return
		}
		sort.Strings(files)
		if len(files) == 0 {
			appendLog("Keine DLL-Dateien im gewählten Ordner gefunden.")
			procPostMessageW.Call(ui.hwnd, WM_APP_DONE, 0, 0)
			return
		}

		appendLog(fmt.Sprintf("Gefunden: %d DLL(s)", len(files)))
		appendLog("Ausgabe: " + outDir)

		// Keep process creation bounded. Four workers are enough to use the PC
		// efficiently without flooding MSVC or the UI with hundreds of processes.
		workers := 4
		if len(files) < workers {
			workers = len(files)
		}

		type task struct {
			index int
			path  string
		}
		jobs := make(chan task)
		var wg sync.WaitGroup
		var success, failed, skipped atomic.Int64

		worker := func() {
			defer wg.Done()
			for t := range jobs {
				dll := t.path
				name := filepath.Base(dll)
				base := strings.TrimSuffix(name, filepath.Ext(name))
				out := filepath.Join(outDir, base+"_proxy.dll")
				forward := base + "_o.dll"

				appendLog(fmt.Sprintf("[%d/%d] %s", t.index+1, len(files), name))
				cmd := exec.Command(builder,
					"build",
					"--dll", dll,
					"--out", out,
					"--forward-module", forward,
				)
				b, err := cmd.CombinedOutput()
				msg := strings.TrimSpace(string(b))
				if err != nil {
					_ = os.Remove(out)
					lower := strings.ToLower(msg)
					if strings.Contains(lower, "no named exports") || strings.Contains(lower, "no exports") {
						skipped.Add(1)
						appendLog("  SKIP " + name + ": keine verwendbaren Exporte")
					} else {
						failed.Add(1)
						appendLog("  FEHLER " + name + ": " + err.Error())
						if msg != "" {
							appendLog(indent(msg, "    "))
						}
					}
					continue
				}

				st, statErr := os.Stat(out)
				if statErr != nil || st.Size() == 0 {
					failed.Add(1)
					appendLog("  FEHLER " + name + ": keine gültige Proxy-DLL erzeugt")
					continue
				}
				success.Add(1)
				appendLog("  OK " + name + " -> " + out)
			}
		}

		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go worker()
		}
		for i, f := range files {
			jobs <- task{index: i, path: f}
		}
		close(jobs)
		wg.Wait()

		appendLog("")
		appendLog("=== FERTIG ===")
		appendLog(fmt.Sprintf("Erfolgreich: %d | Fehler: %d | Übersprungen: %d",
			success.Load(), failed.Load(), skipped.Load()))
		procPostMessageW.Call(ui.hwnd, WM_APP_DONE, 0, 0)
	}()
}

func extractEmbeddedProxyBuilder() (string, func(), error) {
	if len(embeddedProxyBuilder) == 0 {
		return "", func() {}, fmt.Errorf("eingebetteter proxybuilder.exe ist leer")
	}

	dir, err := os.MkdirTemp("", "ProxyBuilderOneFile_")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	path := filepath.Join(dir, "proxybuilder.exe")
	if err := os.WriteFile(path, embeddedProxyBuilder, 0700); err != nil {
		cleanup()
		return "", func() {}, err
	}

	// Verify bytes were written completely before workers start.
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, embeddedProxyBuilder) {
		cleanup()
		return "", func() {}, fmt.Errorf("eingebetteter ProxyBuilder konnte nicht verifiziert werden")
	}
	return path, cleanup, nil
}

func extractEmbeddedIcon() (string, error) {
	if len(embeddedIcon) == 0 {
		return "", fmt.Errorf("eingebettetes Icon ist leer")
	}
	dir, err := os.MkdirTemp("", "ProxyBuilderIcon_")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "proxybuilder.ico")
	if err := os.WriteFile(path, embeddedIcon, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

func appendLog(s string) {
	ui.logMu.Lock()
	if ui.logPending.Len() > 0 {
		ui.logPending.WriteString("\r\n")
	}
	ui.logPending.WriteString(s)
	ui.logMu.Unlock()
}

func flushPendingLog() {
	if ui.hLog == 0 {
		return
	}
	ui.logMu.Lock()
	if ui.logPending.Len() == 0 {
		ui.logMu.Unlock()
		return
	}
	chunk := ui.logPending.String()
	ui.logPending.Reset()
	ui.logMu.Unlock()

	// Append only the new chunk instead of rewriting the complete log.
	negOne := ^uintptr(0)
	procSendMessageW.Call(ui.hLog, EM_SETSEL, negOne, negOne)
	p := wstr(chunk + "\r\n")
	procSendMessageW.Call(ui.hLog, EM_REPLACESEL, 0, uintptr(unsafe.Pointer(p)))
	procSendMessageW.Call(ui.hLog, EM_SCROLLCARET, 0, 0)
}

func setText(h uintptr, s string) {
	if h == 0 {
		return
	}
	procSetWindowTextW.Call(h, uintptr(unsafe.Pointer(wstr(s))))
}

func getText(h uintptr) string {
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 32768)
	procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func indent(s, prefix string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
