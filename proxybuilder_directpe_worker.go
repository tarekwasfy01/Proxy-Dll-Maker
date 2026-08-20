package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "1.0.0"

const (
	imageDOSSignature = 0x5A4D
	imageNTSignature  = 0x00004550
	machineAMD64      = 0x8664
	pe32PlusMagic     = 0x20B
	pe32Magic         = 0x10B
)

type section struct {
	Name        string
	VirtualSize uint32
	VirtualAddr uint32
	RawSize     uint32
	RawPtr      uint32
}

type exportInfo struct {
	DLLName       string              `json:"dll_name"`
	Machine       string              `json:"machine"`
	OrdinalBase   uint32              `json:"ordinal_base"`
	FunctionCount uint32              `json:"function_count"`
	ActiveCount   int                 `json:"active_count"`
	NamedCount    int                 `json:"named_count"`
	Names         map[uint32][]string `json:"names_by_ordinal,omitempty"`
	activeSlots   []bool
}

type result struct {
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Input         string `json:"input,omitempty"`
	Output        string `json:"output,omitempty"`
	ForwardModule string `json:"forward_module,omitempty"`
	Machine       string `json:"machine,omitempty"`
	OrdinalBase   uint32 `json:"ordinal_base,omitempty"`
	FunctionCount uint32 `json:"function_count,omitempty"`
	ActiveCount   int    `json:"active_count,omitempty"`
	NamedCount    int    `json:"named_count,omitempty"`
	Error         string `json:"error,omitempty"`
}

func main() {
	dllDir := workerArg("--dll-dir")
	outDir := workerArg("--out-dir")
	statusPath := workerArg("--status")
	cancelPath := workerArg("--cancel")
	logPath := workerArg("--log")

	if dllDir == "" || outDir == "" || statusPath == "" || cancelPath == "" || logPath == "" {
		return
	}
	_ = os.MkdirAll(outDir, 0755)

	logFile, err := os.Create(logPath)
	if err != nil {
		workerStatus(statusPath, "ERROR|Cannot create log file")
		return
	}
	defer logFile.Close()

	logLine := func(s string) {
		_, _ = fmt.Fprintln(logFile, s)
	}
	logLine("ProxyBuilder Direct-PE v3")
	logLine("Architecture: GUI process -> isolated worker process -> direct PE forwarder generation")
	logLine("No MSVC, no link.exe, no cl.exe, no dumpbin.exe.")

	all, err := filepath.Glob(filepath.Join(dllDir, "*.dll"))
	if err != nil {
		logLine("ERROR: " + err.Error())
		workerStatus(statusPath, "ERROR|"+workerStatusEscape(err.Error()))
		return
	}
	sort.Strings(all)

	files := make([]string, 0, len(all))
	for _, path := range all {
		name := strings.ToLower(filepath.Base(path))
		if strings.HasSuffix(name, "_proxy.dll") ||
			strings.HasSuffix(name, "_o.dll") ||
			name == "universal_proxy.dll" {
			logLine("SKIP generated/support DLL: " + filepath.Base(path))
			continue
		}
		files = append(files, path)
	}

	workerStatus(statusPath, fmt.Sprintf("READY|%d", len(files)))
	logLine(fmt.Sprintf("Input DLLs: %d", len(files)))

	created, failed, skipped := 0, 0, 0

	for i, dll := range files {
		if workerCancelled(cancelPath) {
			logLine("CANCELLED")
			workerStatus(statusPath, "CANCELLED")
			return
		}

		name := filepath.Base(dll)
		base := strings.TrimSuffix(name, filepath.Ext(name))
		out := filepath.Join(outDir, base+"_proxy.dll")
		forwardModule := normalizeModule(base + "_o")

		workerStatus(statusPath, fmt.Sprintf("RUN|%d|%d|%s", i+1, len(files), name))
		logLine(fmt.Sprintf("[%d/%d] %s", i+1, len(files), name))

		info, err := parseExports(dll)
		if err != nil {
			failed++
			logLine("  ERROR parse exports: " + err.Error())
			continue
		}
		if info.Machine != "x64" {
			skipped++
			logLine("  SKIP unsupported machine: " + info.Machine)
			continue
		}
		if info.ActiveCount == 0 {
			skipped++
			logLine("  SKIP no active exports")
			continue
		}

		blob, err := buildForwarderDLL(info, filepath.Base(out), forwardModule)
		if err != nil {
			failed++
			logLine("  ERROR build: " + err.Error())
			continue
		}

		tmp := out + ".tmp"
		if err := os.WriteFile(tmp, blob, 0644); err != nil {
			failed++
			logLine("  ERROR write: " + err.Error())
			continue
		}
		_ = os.Remove(out)
		if err := os.Rename(tmp, out); err != nil {
			_ = os.Remove(tmp)
			failed++
			logLine("  ERROR replace output: " + err.Error())
			continue
		}

		// Structural self-check.
		built, err := parseExports(out)
		if err != nil ||
			built.OrdinalBase != info.OrdinalBase ||
			built.FunctionCount != info.FunctionCount ||
			built.ActiveCount != info.ActiveCount ||
			built.NamedCount != info.NamedCount {
			_ = os.Remove(out)
			failed++
			if err != nil {
				logLine("  ERROR verify: " + err.Error())
			} else {
				logLine("  ERROR verify: generated export table differs from source")
			}
			continue
		}

		created++
		logLine("  OK -> " + out)
	}

	logLine("")
	logLine("=== FINISHED ===")
	logLine(fmt.Sprintf("Created: %d | Failed: %d | Skipped: %d", created, failed, skipped))
	workerStatus(statusPath, fmt.Sprintf("DONE|%d|%d|%d", created, failed, skipped))
}

func workerArg(name string) string {
	for i := 1; i+1 < len(os.Args); i++ {
		if strings.EqualFold(os.Args[i], name) {
			return os.Args[i+1]
		}
	}
	return ""
}

func workerStatus(path, value string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0644); err == nil {
		_ = os.Remove(path)
		_ = os.Rename(tmp, path)
	}
}

func workerCancelled(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

func workerStatusEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func usage() {
	fmt.Printf(`ProxyBuilder %s - standalone PE export-forwarder builder (x64 output)

USAGE
  proxybuilder.exe build   --dll <source.dll> --out <proxy.dll> --forward-module <renamed_original>
  proxybuilder.exe inspect --dll <file.dll> [--json]
  proxybuilder.exe verify  --dll <file.dll> [--json]
  proxybuilder.exe version

EXAMPLE
  proxybuilder.exe build --dll "C:\\Windows\\System32\\AarSvc.dll" --out "C:\\Temp\\AarSvc.dll" --forward-module "AarSvc_o"

The generated proxy contains PE export forwarders only. The original DLL must be
available under --forward-module (for example AarSvc_o.dll) when the proxy loads.
`, version)
}

func runBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("dll", "", "source DLL whose exports are mirrored")
	out := fs.String("out", "", "output proxy DLL")
	forward := fs.String("forward-module", "", "module that receives forwarded exports, e.g. AarSvc_o")
	asJSON := fs.Bool("json", false, "emit JSON result")
	if err := fs.Parse(args); err != nil {
		return emitError(*asJSON, "build", "", "", "", err)
	}
	if strings.TrimSpace(*in) == "" || strings.TrimSpace(*out) == "" || strings.TrimSpace(*forward) == "" {
		return emitError(*asJSON, "build", *in, *out, *forward, errors.New("--dll, --out and --forward-module are required"))
	}

	forwardModule := normalizeModule(*forward)
	if forwardModule == "" {
		return emitError(*asJSON, "build", *in, *out, *forward, errors.New("invalid --forward-module"))
	}

	info, err := parseExports(*in)
	if err != nil {
		return emitError(*asJSON, "build", *in, *out, forwardModule, err)
	}
	if info.Machine != "x64" {
		return emitError(*asJSON, "build", *in, *out, forwardModule, fmt.Errorf("source machine is %s; v1.0 builds x64 proxies only", info.Machine))
	}
	if info.ActiveCount == 0 {
		return emitError(*asJSON, "build", *in, *out, forwardModule, errors.New("source DLL has no active exports"))
	}

	dllName := filepath.Base(*out)
	blob, err := buildForwarderDLL(info, dllName, forwardModule)
	if err != nil {
		return emitError(*asJSON, "build", *in, *out, forwardModule, err)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil && filepath.Dir(*out) != "." {
		return emitError(*asJSON, "build", *in, *out, forwardModule, err)
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, blob, 0644); err != nil {
		return emitError(*asJSON, "build", *in, *out, forwardModule, err)
	}
	_ = os.Remove(*out)
	if err := os.Rename(tmp, *out); err != nil {
		_ = os.Remove(tmp)
		return emitError(*asJSON, "build", *in, *out, forwardModule, err)
	}

	// Re-parse our own output as a structural sanity check.
	built, err := parseExports(*out)
	if err != nil {
		_ = os.Remove(*out)
		return emitError(*asJSON, "build", *in, *out, forwardModule, fmt.Errorf("generated DLL failed self-check: %w", err))
	}
	if built.OrdinalBase != info.OrdinalBase || built.FunctionCount != info.FunctionCount || built.ActiveCount != info.ActiveCount || built.NamedCount != info.NamedCount {
		_ = os.Remove(*out)
		return emitError(*asJSON, "build", *in, *out, forwardModule, errors.New("generated DLL export table differs from source"))
	}

	r := result{OK: true, Command: "build", Input: *in, Output: *out, ForwardModule: forwardModule, Machine: built.Machine, OrdinalBase: built.OrdinalBase, FunctionCount: built.FunctionCount, ActiveCount: built.ActiveCount, NamedCount: built.NamedCount}
	emit(*asJSON, r)
	return 0
}

func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("dll", "", "DLL to inspect")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return emitError(*asJSON, "inspect", *in, "", "", err)
	}
	if *in == "" {
		return emitError(*asJSON, "inspect", "", "", "", errors.New("--dll is required"))
	}
	info, err := parseExports(*in)
	if err != nil {
		return emitError(*asJSON, "inspect", *in, "", "", err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(info, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("DLL: %s\nMachine: %s\nOrdinal base: %d\nFunction slots: %d\nActive exports: %d\nNamed exports: %d\n", info.DLLName, info.Machine, info.OrdinalBase, info.FunctionCount, info.ActiveCount, info.NamedCount)
	ords := make([]int, 0, len(info.Names))
	for ord := range info.Names {
		ords = append(ords, int(ord))
	}
	sort.Ints(ords)
	for _, oi := range ords {
		names := append([]string(nil), info.Names[uint32(oi)]...)
		sort.Strings(names)
		fmt.Printf("  @%d  %s\n", oi, strings.Join(names, ", "))
	}
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("dll", "", "DLL to verify")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return emitError(*asJSON, "verify", *in, "", "", err)
	}
	if *in == "" {
		return emitError(*asJSON, "verify", "", "", "", errors.New("--dll is required"))
	}
	info, err := parseExports(*in)
	if err != nil {
		return emitError(*asJSON, "verify", *in, "", "", err)
	}
	r := result{OK: true, Command: "verify", Input: *in, Machine: info.Machine, OrdinalBase: info.OrdinalBase, FunctionCount: info.FunctionCount, ActiveCount: info.ActiveCount, NamedCount: info.NamedCount}
	emit(*asJSON, r)
	return 0
}

func emitError(asJSON bool, cmd, in, out, fwd string, err error) int {
	r := result{OK: false, Command: cmd, Input: in, Output: out, ForwardModule: fwd, Error: err.Error()}
	emit(asJSON, r)
	return 1
}

func emit(asJSON bool, r result) {
	if asJSON {
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		return
	}
	if !r.OK {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", r.Error)
		return
	}
	switch r.Command {
	case "build":
		fmt.Printf("OK\nOutput: %s\nForward module: %s.dll\nMachine: %s\nOrdinal base: %d\nFunction slots: %d\nActive exports: %d\nNamed exports: %d\n", r.Output, r.ForwardModule, r.Machine, r.OrdinalBase, r.FunctionCount, r.ActiveCount, r.NamedCount)
	case "verify":
		fmt.Printf("OK: valid PE export table\nMachine: %s\nOrdinal base: %d\nFunction slots: %d\nActive exports: %d\nNamed exports: %d\n", r.Machine, r.OrdinalBase, r.FunctionCount, r.ActiveCount, r.NamedCount)
	}
}

func normalizeModule(s string) string {
	s = strings.TrimSpace(s)
	s = filepath.Base(s)
	if strings.EqualFold(filepath.Ext(s), ".dll") {
		s = strings.TrimSuffix(s, filepath.Ext(s))
	}
	if s == "." || s == "" || strings.ContainsAny(s, "\x00\r\n") {
		return ""
	}
	return s
}

func parseExports(path string) (*exportInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 0x100 {
		return nil, errors.New("file too small for PE")
	}
	if binary.LittleEndian.Uint16(data[0:2]) != imageDOSSignature {
		return nil, errors.New("missing MZ header")
	}
	peOff := int(binary.LittleEndian.Uint32(data[0x3c:0x40]))
	if peOff < 0 || peOff+24 > len(data) {
		return nil, errors.New("invalid PE header offset")
	}
	if binary.LittleEndian.Uint32(data[peOff:peOff+4]) != imageNTSignature {
		return nil, errors.New("missing PE signature")
	}

	coff := peOff + 4
	machine := binary.LittleEndian.Uint16(data[coff : coff+2])
	numSections := int(binary.LittleEndian.Uint16(data[coff+2 : coff+4]))
	optSize := int(binary.LittleEndian.Uint16(data[coff+16 : coff+18]))
	opt := coff + 20
	if opt+optSize > len(data) {
		return nil, errors.New("truncated optional header")
	}
	magic := binary.LittleEndian.Uint16(data[opt : opt+2])
	var ddOff int
	var machineName string
	switch machine {
	case machineAMD64:
		machineName = "x64"
	case 0x14c:
		machineName = "x86"
	case 0xAA64:
		machineName = "arm64"
	default:
		machineName = fmt.Sprintf("0x%04X", machine)
	}
	switch magic {
	case pe32PlusMagic:
		ddOff = opt + 112
	case pe32Magic:
		ddOff = opt + 96
	default:
		return nil, fmt.Errorf("unsupported optional header magic 0x%X", magic)
	}
	if ddOff+8 > opt+optSize {
		return nil, errors.New("optional header has no export data directory")
	}
	exportRVA := binary.LittleEndian.Uint32(data[ddOff : ddOff+4])
	exportSize := binary.LittleEndian.Uint32(data[ddOff+4 : ddOff+8])

	secOff := opt + optSize
	if numSections < 0 || numSections > 96 || secOff+numSections*40 > len(data) {
		return nil, errors.New("invalid section table")
	}
	secs := make([]section, 0, numSections)
	for i := 0; i < numSections; i++ {
		p := secOff + i*40
		rawName := data[p : p+8]
		if z := bytes.IndexByte(rawName, 0); z >= 0 {
			rawName = rawName[:z]
		}
		secs = append(secs, section{
			Name:        string(rawName),
			VirtualSize: binary.LittleEndian.Uint32(data[p+8 : p+12]),
			VirtualAddr: binary.LittleEndian.Uint32(data[p+12 : p+16]),
			RawSize:     binary.LittleEndian.Uint32(data[p+16 : p+20]),
			RawPtr:      binary.LittleEndian.Uint32(data[p+20 : p+24]),
		})
	}

	info := &exportInfo{DLLName: filepath.Base(path), Machine: machineName, Names: make(map[uint32][]string)}
	if exportRVA == 0 || exportSize == 0 {
		return info, nil
	}
	expOff, err := rvaToOffset(exportRVA, secs, len(data))
	if err != nil {
		return nil, fmt.Errorf("export directory: %w", err)
	}
	if expOff+40 > len(data) {
		return nil, errors.New("truncated export directory")
	}

	nameRVA := binary.LittleEndian.Uint32(data[expOff+12 : expOff+16])
	base := binary.LittleEndian.Uint32(data[expOff+16 : expOff+20])
	numFuncs := binary.LittleEndian.Uint32(data[expOff+20 : expOff+24])
	numNames := binary.LittleEndian.Uint32(data[expOff+24 : expOff+28])
	addrFuncs := binary.LittleEndian.Uint32(data[expOff+28 : expOff+32])
	addrNames := binary.LittleEndian.Uint32(data[expOff+32 : expOff+36])
	addrOrds := binary.LittleEndian.Uint32(data[expOff+36 : expOff+40])
	if numFuncs > 1_000_000 || numNames > 1_000_000 {
		return nil, errors.New("unreasonable export count")
	}

	info.OrdinalBase = base
	info.FunctionCount = numFuncs
	info.activeSlots = make([]bool, int(numFuncs))
	if nameRVA != 0 {
		if no, e := rvaToOffset(nameRVA, secs, len(data)); e == nil {
			if s, e := readCString(data, no); e == nil && s != "" {
				info.DLLName = s
			}
		}
	}

	if numFuncs > 0 {
		fo, err := rvaToOffset(addrFuncs, secs, len(data))
		if err != nil {
			return nil, fmt.Errorf("function table: %w", err)
		}
		if fo+int(numFuncs)*4 > len(data) {
			return nil, errors.New("truncated function table")
		}
		for i := uint32(0); i < numFuncs; i++ {
			if binary.LittleEndian.Uint32(data[fo+int(i)*4:fo+int(i)*4+4]) != 0 {
				info.activeSlots[i] = true
				info.ActiveCount++
			}
		}
	}

	if numNames > 0 {
		no, err := rvaToOffset(addrNames, secs, len(data))
		if err != nil {
			return nil, fmt.Errorf("name table: %w", err)
		}
		oo, err := rvaToOffset(addrOrds, secs, len(data))
		if err != nil {
			return nil, fmt.Errorf("ordinal table: %w", err)
		}
		if no+int(numNames)*4 > len(data) || oo+int(numNames)*2 > len(data) {
			return nil, errors.New("truncated export name tables")
		}
		for i := uint32(0); i < numNames; i++ {
			nrva := binary.LittleEndian.Uint32(data[no+int(i)*4 : no+int(i)*4+4])
			slot := uint32(binary.LittleEndian.Uint16(data[oo+int(i)*2 : oo+int(i)*2+2]))
			if slot >= numFuncs {
				return nil, errors.New("export name ordinal outside function table")
			}
			nfo, err := rvaToOffset(nrva, secs, len(data))
			if err != nil {
				return nil, fmt.Errorf("export name: %w", err)
			}
			name, err := readCString(data, nfo)
			if err != nil {
				return nil, err
			}
			ord := base + slot
			info.Names[ord] = append(info.Names[ord], name)
			info.NamedCount++
		}
	}
	return info, nil
}

func rvaToOffset(rva uint32, secs []section, fileLen int) (int, error) {
	for _, s := range secs {
		span := s.VirtualSize
		if s.RawSize > span {
			span = s.RawSize
		}
		if rva >= s.VirtualAddr && rva < s.VirtualAddr+span {
			delta := rva - s.VirtualAddr
			off := uint64(s.RawPtr) + uint64(delta)
			if off >= uint64(fileLen) {
				return 0, errors.New("RVA maps beyond file")
			}
			return int(off), nil
		}
	}
	// Header RVA fallback.
	if int(rva) < fileLen {
		return int(rva), nil
	}
	return 0, fmt.Errorf("RVA 0x%X is not mapped", rva)
}

func readCString(data []byte, off int) (string, error) {
	if off < 0 || off >= len(data) {
		return "", errors.New("string offset outside file")
	}
	end := off
	for end < len(data) && data[end] != 0 {
		end++
		if end-off > 1<<20 {
			return "", errors.New("unterminated/oversized string")
		}
	}
	if end >= len(data) {
		return "", errors.New("unterminated string")
	}
	return string(data[off:end]), nil
}

type namedExport struct {
	Name string
	Slot uint16
}

func buildForwarderDLL(src *exportInfo, dllName, forwardModule string) ([]byte, error) {
	if src.FunctionCount == 0 || len(src.activeSlots) != int(src.FunctionCount) {
		return nil, errors.New("invalid source export layout")
	}
	if src.FunctionCount > 65536 {
		return nil, errors.New("function table too large for PE name ordinals")
	}

	const sectionRVA = uint32(0x1000)
	const fileAlign = uint32(0x200)
	const sectionAlign = uint32(0x1000)
	const headersSize = uint32(0x200)

	named := make([]namedExport, 0, src.NamedCount)
	for ord, names := range src.Names {
		if ord < src.OrdinalBase {
			return nil, errors.New("invalid named export ordinal")
		}
		slot := ord - src.OrdinalBase
		if slot >= src.FunctionCount || slot > 0xFFFF {
			return nil, errors.New("named export slot out of range")
		}
		for _, n := range names {
			named = append(named, namedExport{Name: n, Slot: uint16(slot)})
		}
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Name < named[j].Name })

	// Layout inside .edata:
	// directory | function RVA table | name RVA table | name ordinal table | strings
	offDir := uint32(0)
	offFuncs := offDir + 40
	offNames := offFuncs + src.FunctionCount*4
	offOrds := offNames + uint32(len(named))*4
	offStrings := align(offOrds+uint32(len(named))*2, 4)

	var stringsBuf bytes.Buffer
	dllNameOff := offStrings + uint32(stringsBuf.Len())
	stringsBuf.WriteString(dllName)
	stringsBuf.WriteByte(0)

	nameOffsets := make([]uint32, len(named))
	for i, n := range named {
		nameOffsets[i] = offStrings + uint32(stringsBuf.Len())
		stringsBuf.WriteString(n.Name)
		stringsBuf.WriteByte(0)
	}

	forwardOffsets := make([]uint32, src.FunctionCount)
	for i := uint32(0); i < src.FunctionCount; i++ {
		if !src.activeSlots[i] {
			continue
		}
		ord := src.OrdinalBase + i
		forwardOffsets[i] = offStrings + uint32(stringsBuf.Len())
		stringsBuf.WriteString(forwardModule)
		stringsBuf.WriteString(".#")
		stringsBuf.WriteString(strconv.FormatUint(uint64(ord), 10))
		stringsBuf.WriteByte(0)
	}

	// Marker used by Home Cluster to recognize an ABI-neutral safe forwarder.
	// It is plain data only and does not add or alter any exported function.
	stringsBuf.WriteString("HC_SAFE_FORWARD_V8")
	stringsBuf.WriteByte(0)

	virtualSize := offStrings + uint32(stringsBuf.Len())
	rawSize := align(virtualSize, fileAlign)
	edata := make([]byte, rawSize)

	put32 := func(off uint32, v uint32) { binary.LittleEndian.PutUint32(edata[off:off+4], v) }
	put16 := func(off uint32, v uint16) { binary.LittleEndian.PutUint16(edata[off:off+2], v) }

	now := uint32(time.Now().Unix())
	put32(offDir+4, now)
	put32(offDir+12, sectionRVA+dllNameOff)
	put32(offDir+16, src.OrdinalBase)
	put32(offDir+20, src.FunctionCount)
	put32(offDir+24, uint32(len(named)))
	put32(offDir+28, sectionRVA+offFuncs)
	put32(offDir+32, sectionRVA+offNames)
	put32(offDir+36, sectionRVA+offOrds)

	for i := uint32(0); i < src.FunctionCount; i++ {
		if forwardOffsets[i] != 0 {
			put32(offFuncs+i*4, sectionRVA+forwardOffsets[i])
		}
	}
	for i, n := range named {
		put32(offNames+uint32(i)*4, sectionRVA+nameOffsets[i])
		put16(offOrds+uint32(i)*2, n.Slot)
	}
	copy(edata[offStrings:], stringsBuf.Bytes())

	imageSize := align(sectionRVA+virtualSize, sectionAlign)
	fileSize := headersSize + rawSize
	out := make([]byte, fileSize)

	// DOS header/stub.
	binary.LittleEndian.PutUint16(out[0:2], imageDOSSignature)
	binary.LittleEndian.PutUint32(out[0x3c:0x40], 0x80)
	copy(out[0x40:], []byte("This program cannot be run in DOS mode.\r\r\n$"))

	pe := uint32(0x80)
	binary.LittleEndian.PutUint32(out[pe:pe+4], imageNTSignature)
	coff := pe + 4
	binary.LittleEndian.PutUint16(out[coff:coff+2], machineAMD64)
	binary.LittleEndian.PutUint16(out[coff+2:coff+4], 1)
	binary.LittleEndian.PutUint32(out[coff+4:coff+8], now)
	binary.LittleEndian.PutUint16(out[coff+16:coff+18], 0xF0)
	binary.LittleEndian.PutUint16(out[coff+18:coff+20], 0x2022) // executable | large address aware | DLL

	opt := coff + 20
	binary.LittleEndian.PutUint16(out[opt:opt+2], pe32PlusMagic)
	out[opt+2] = 1                                                 // linker major
	binary.LittleEndian.PutUint32(out[opt+8:opt+12], rawSize)      // initialized data
	binary.LittleEndian.PutUint64(out[opt+24:opt+32], 0x180000000) // image base
	binary.LittleEndian.PutUint32(out[opt+32:opt+36], sectionAlign)
	binary.LittleEndian.PutUint32(out[opt+36:opt+40], fileAlign)
	binary.LittleEndian.PutUint16(out[opt+40:opt+42], 6) // OS major
	binary.LittleEndian.PutUint16(out[opt+48:opt+50], 6) // subsystem major
	binary.LittleEndian.PutUint32(out[opt+56:opt+60], imageSize)
	binary.LittleEndian.PutUint32(out[opt+60:opt+64], headersSize)
	binary.LittleEndian.PutUint16(out[opt+68:opt+70], 3)      // Windows CUI
	binary.LittleEndian.PutUint16(out[opt+70:opt+72], 0x8160) // ASLR/NX/high entropy/terminal server aware
	binary.LittleEndian.PutUint64(out[opt+72:opt+80], 1<<20)
	binary.LittleEndian.PutUint64(out[opt+80:opt+88], 1<<12)
	binary.LittleEndian.PutUint64(out[opt+88:opt+96], 1<<20)
	binary.LittleEndian.PutUint64(out[opt+96:opt+104], 1<<12)
	binary.LittleEndian.PutUint32(out[opt+108:opt+112], 16)
	// Export data directory.
	binary.LittleEndian.PutUint32(out[opt+112:opt+116], sectionRVA)
	binary.LittleEndian.PutUint32(out[opt+116:opt+120], virtualSize)

	sec := opt + 0xF0
	copy(out[sec:sec+8], []byte(".edata\x00\x00"))
	binary.LittleEndian.PutUint32(out[sec+8:sec+12], virtualSize)
	binary.LittleEndian.PutUint32(out[sec+12:sec+16], sectionRVA)
	binary.LittleEndian.PutUint32(out[sec+16:sec+20], rawSize)
	binary.LittleEndian.PutUint32(out[sec+20:sec+24], headersSize)
	binary.LittleEndian.PutUint32(out[sec+36:sec+40], 0x40000040) // initialized data | read

	copy(out[headersSize:], edata)
	return out, nil
}

func align(v, a uint32) uint32 {
	return (v + a - 1) &^ (a - 1)
}
