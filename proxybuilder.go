// proxybuilder_homecluster_allcompute_testfolder.go
// HomeCluster-compatible local-test proxy builder for Windows x64.
//
// Safety model:
//   - Works only on a user-selected local test DLL folder.
//   - Refuses Windows/System32/SysWOW64 source folders.
//   - Unknown named exports are ABI-neutral PE forwarders by NAME to <name>_o.dll.
//   - Optional HomeCluster notification is limited to DllCanUnloadNow,
//     whose COM signature is exactly HRESULT WINAPI DllCanUnloadNow(void).
//
// Build:
//   go build -o build_proxy_wrappers_safe_v4.exe build_proxy_wrappers_safe_v4.go
//
// Example:
//   build_proxy_wrappers_safe_v4.exe ^
//     --dll-dir "C:\Users\you\Desktop\test_dlls" ^
//     --out-dir "C:\Users\you\Desktop\shims" ^
//     --universal "C:\Users\you\Desktop\universal_proxy.dll" ^
//     --notify-dllcanunloadnow
//
// Runtime layout for a controlled test application:
//   foo.dll              <- generated foo_proxy.dll renamed to foo.dll
//   foo_o.dll            <- original foo.dll
//   universal_proxy.dll  <- only needed when notification is enabled
//
// Requires MSVC x64 Build Tools and Windows SDK.

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	clPath      string
	linkPath    string
	dumpbinPath string
	includePath []string
	libPath     []string
)

type Export struct {
	Ordinal int
	Name    string
}

type ComputeRoute struct {
	Kind              string
	Operation         string
	CanonicalLibrary  string
	CanonicalFunction string
}

type WrappedExport struct {
	Export     Export
	Route      ComputeRoute
	HelperName string
}

func main() {
	// HomeCluster's existing runtime calls the builder in single-DLL CLI mode:
	//   proxybuilder.exe build --dll <source> --out <proxy> --forward-module <name_o.dll>
	// Keep that contract first. Folder mode remains available for manual batch tests.
	if len(os.Args) > 1 && strings.EqualFold(os.Args[1], "build") {
		if err := runSingleBuild(os.Args[2:]); err != nil {
			fail(err)
		}
		return
	}

	dllDir := flag.String("dll-dir", `C:\Users\tarek\Desktop\test_dlls`, "Controlled local folder containing source DLLs")
	outDir := flag.String("out-dir", `C:\Users\tarek\Desktop\shims`, "Output folder")
	flag.Parse()

	if strings.TrimSpace(*dllDir) == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --dll-dir is required")
		flag.Usage()
		os.Exit(2)
	}

	srcDir, err := filepath.Abs(filepath.Clean(*dllDir))
	if err != nil {
		fail(err)
	}
	if err := rejectProtectedWindowsFolder(srcDir); err != nil {
		fail(err)
	}
	dstDir, err := filepath.Abs(filepath.Clean(*outDir))
	if err != nil {
		fail(err)
	}

	st, err := os.Stat(srcDir)
	if err != nil || !st.IsDir() {
		fail(fmt.Errorf("DLL folder does not exist or is not a directory: %s", srcDir))
	}
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		fail(err)
	}
	if err := findMSVC(); err != nil {
		fail(err)
	}

	logPath := filepath.Join(dstDir, "ProxyBuilder_HomeCluster.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		fail(err)
	}
	defer logFile.Close()
	logLine := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		fmt.Println(s)
		_, _ = fmt.Fprintln(logFile, s)
	}

	files, err := filepath.Glob(filepath.Join(srcDir, "*.dll"))
	if err != nil {
		fail(err)
	}
	if len(files) == 0 {
		fail(fmt.Errorf("no DLLs found in %s", srcDir))
	}
	sort.Strings(files)

	logLine("ProxyBuilder HomeCluster Compute Bridge")
	logLine(`Pipe: \\.\pipe\HomeCluster-Compute`)
	logLine("HomeCluster pipe routes: 15 payload-based structured compute operations (prime-count excluded: ranged job contract)")
	logLine("")

	ok, skipped, failed := 0, 0, 0
	for _, dll := range files {
		base := strings.ToLower(filepath.Base(dll))
		if strings.HasSuffix(base, "_proxy.dll") || strings.HasSuffix(base, "_o.dll") || strings.HasSuffix(base, "_hcwrap.dll") || base == "universal_proxy.dll" {
			skipped++
			logLine("SKIP %s", filepath.Base(dll))
			continue
		}
		wrapped, err := buildProxy(dll, dstDir)
		if err != nil {
			failed++
			logLine("FAILED %s: %v", filepath.Base(dll), err)
		} else {
			ok++
			logLine("OK %s wrapped=%d", filepath.Base(dll), wrapped)
		}
	}
	logLine("")
	logLine("Done. OK=%d SKIP=%d FAILED=%d", ok, skipped, failed)
	logLine("Log: %s", logPath)
	if failed != 0 {
		os.Exit(1)
	}
}

func runSingleBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dllArg := fs.String("dll", "", "source DLL")
	outArg := fs.String("out", "", "output proxy DLL")
	forwardArg := fs.String("forward-module", "", "renamed original module, e.g. version_o.dll")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dllArg) == "" || strings.TrimSpace(*outArg) == "" {
		return errors.New("usage: proxybuilder.exe build --dll <source.dll> --out <proxy.dll> [--forward-module <name_o.dll>]")
	}

	outPath, err := filepath.Abs(filepath.Clean(*outArg))
	if err != nil {
		return err
	}
	outDir := filepath.Dir(outPath)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	forwardModule := strings.TrimSpace(*forwardArg)
	if forwardModule == "" {
		base := strings.TrimSuffix(filepath.Base(*dllArg), filepath.Ext(*dllArg))
		forwardModule = base + "_o.dll"
	}
	forwardModule = filepath.Base(forwardModule)
	forwardBase := strings.TrimSuffix(forwardModule, filepath.Ext(forwardModule))
	if forwardBase == "" || forwardBase == "." {
		return errors.New("invalid --forward-module")
	}

	// Every generated proxy is a self-contained deployment pair: the proxy and
	// the copied/renamed original live beside one another. This keeps protected
	// Windows folders read-only and gives HomeCluster an unambiguous forward DLL.
	localOriginal := filepath.Join(outDir, forwardModule)
	sourceDLL := localOriginal
	if st, statErr := os.Stat(localOriginal); statErr != nil || st.IsDir() {
		requestedSource, absErr := filepath.Abs(filepath.Clean(*dllArg))
		if absErr != nil {
			return absErr
		}
		if st, statErr := os.Stat(requestedSource); statErr != nil || st.IsDir() {
			return fmt.Errorf("source DLL does not exist: %s", requestedSource)
		}
		if copyErr := copyFile(requestedSource, localOriginal); copyErr != nil {
			return fmt.Errorf("copy original DLL %s: %w", requestedSource, copyErr)
		}
		sourceDLL = localOriginal
		fmt.Printf("COPIED %s -> %s\n", requestedSource, localOriginal)
	}
	if st, err := os.Stat(sourceDLL); err != nil || st.IsDir() {
		return fmt.Errorf("source DLL does not exist: %s", sourceDLL)
	}

	if err := findMSVC(); err != nil {
		return err
	}
	wrapped, err := buildProxyTo(sourceDLL, outPath, forwardBase, false)
	if err != nil {
		return err
	}
	fmt.Printf("BUILT %s\n", outPath)
	fmt.Printf("SOURCE %s\n", sourceDLL)
	fmt.Printf("FORWARD %s.dll\n", forwardBase)
	fmt.Printf("PIPE \\\\.\\pipe\\HomeCluster-Compute\n")
	fmt.Printf("WRAPPED %d\n", wrapped)
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

func rejectProtectedWindowsFolder(dir string) error {
	clean := strings.TrimRight(strings.ToLower(filepath.Clean(dir)), `\/`)
	win := strings.TrimRight(strings.ToLower(filepath.Clean(os.Getenv("WINDIR"))), `\/`)
	if win == "" || win == "." {
		win = strings.ToLower(filepath.Clean(`C:\Windows`))
	}
	blocked := []string{
		win,
		filepath.Join(win, "system32"),
		filepath.Join(win, "syswow64"),
		filepath.Join(win, "winsxs"),
	}
	for _, b := range blocked {
		b = strings.TrimRight(strings.ToLower(filepath.Clean(b)), `\/`)
		if clean == b || strings.HasPrefix(clean, b+string(os.PathSeparator)) {
			return fmt.Errorf("refusing protected Windows folder: %s; copy DLLs to a controlled test folder first", dir)
		}
	}
	return nil
}

func buildProxy(dll, outDir string) (int, error) {
	base := strings.TrimSuffix(filepath.Base(dll), filepath.Ext(dll))
	out := filepath.Join(outDir, base+"_proxy.dll")
	wrapped, err := buildProxyTo(dll, out, base+"_o", true)
	return wrapped, err
}

func buildProxyTo(dll, out, forwardBase string, copyOriginal bool) (int, error) {
	exports, err := readNamedExports(dll)
	if err != nil {
		return 0, err
	}
	if len(exports) == 0 {
		return 0, fmt.Errorf("no named exports")
	}

	outDir := filepath.Dir(out)
	wrapped := make([]WrappedExport, 0)
	seenOrdinal := make(map[int]bool)
	for _, e := range exports {
		if seenOrdinal[e.Ordinal] {
			continue
		}
		if route, ok := routeForExport(e.Name); ok {
			wrapped = append(wrapped, WrappedExport{
				Export:     e,
				Route:      route,
				HelperName: fmt.Sprintf("HCW_%d", e.Ordinal),
			})
			seenOrdinal[e.Ordinal] = true
		}
	}

	if len(wrapped) != 0 {
		if err := buildComputeProxy(out, exports, wrapped, forwardBase); err != nil {
			return 0, err
		}
	} else {
		blob, err := buildNamedForwarderPE(exports, filepath.Base(out), forwardBase, nil)
		if err != nil {
			return 0, err
		}
		tmp := out + ".tmp"
		if err := os.WriteFile(tmp, blob, 0644); err != nil {
			return 0, err
		}
		_ = os.Remove(out)
		if err := os.Rename(tmp, out); err != nil {
			_ = os.Remove(tmp)
			return 0, err
		}
	}

	got, err := readNamedExports(out)
	if err != nil {
		return 0, fmt.Errorf("verify exports: %w", err)
	}
	if len(got) != len(exports) {
		return 0, fmt.Errorf("verify export count mismatch: source=%d proxy=%d", len(exports), len(got))
	}

	bin, err := os.ReadFile(out)
	if err != nil {
		return 0, err
	}
	if !bytes.Contains(bin, []byte("HC_SAFE_FORWARD_HC_PIPE_V2")) {
		return 0, fmt.Errorf("verify failed: safe marker missing")
	}
	if len(wrapped) != len(exports) && !bytes.Contains(bytes.ToLower(bin), bytes.ToLower([]byte(forwardBase))) {
		return 0, fmt.Errorf("verify failed: forward module %q not embedded", forwardBase)
	}

	if copyOriginal {
		originalCopy := filepath.Join(outDir, forwardBase+".dll")
		if err := copyFile(dll, originalCopy); err != nil {
			return 0, fmt.Errorf("copy original: %w", err)
		}
	}

	for _, w := range wrapped {
		fmt.Printf("  PIPE %s -> %s/%s\n", w.Export.Name, w.Route.CanonicalLibrary, w.Route.CanonicalFunction)
	}
	return len(wrapped), nil
}

func routeForExport(name string) (ComputeRoute, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "hc_vectoradd", "vector_add", "vector-add", "add":
		return ComputeRoute{Kind: "vector_add", Operation: "vector-add", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_VectorAdd"}, true
	case "hc_vectorsub", "vector_sub", "vector-sub":
		return ComputeRoute{Kind: "vector_sub", Operation: "vector-sub", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_VectorSub"}, true
	case "hc_vectormul", "vector_mul", "vector-mul":
		return ComputeRoute{Kind: "vector_mul", Operation: "vector-mul", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_VectorMul"}, true
	case "hc_vectordot", "vector_dot", "vector-dot":
		return ComputeRoute{Kind: "vector_dot", Operation: "vector-dot", CanonicalLibrary: "mkl_rt.dll", CanonicalFunction: "HC_VectorDot"}, true
	case "hc_vectorscale", "vector_scale", "vector-scale":
		return ComputeRoute{Kind: "vector_scale", Operation: "vector-scale", CanonicalLibrary: "opencl.dll", CanonicalFunction: "vector-scale"}, true
	case "hc_reducesum", "reduce_sum", "reduce-sum":
		return ComputeRoute{Kind: "reduce", Operation: "reduce-sum", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_ReduceSum"}, true
	case "hc_reducemin", "reduce_min", "reduce-min":
		return ComputeRoute{Kind: "reduce", Operation: "reduce-min", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_ReduceMin"}, true
	case "hc_reducemax", "reduce_max", "reduce-max":
		return ComputeRoute{Kind: "reduce", Operation: "reduce-max", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_ReduceMax"}, true
	case "hc_reducesumsq", "reduce_sumsq", "reduce-sumsq":
		return ComputeRoute{Kind: "reduce", Operation: "reduce-sumsq", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_ReduceSumSq"}, true
	case "hc_matrixmultiply", "matrix_multiply", "matrix-mul-rows", "matmul":
		return ComputeRoute{Kind: "matrix_multiply", Operation: "matrix-mul-rows", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_MatrixMultiply"}, true
	case "hc_matrixvector", "matrix_vector", "matrix-vector-rows", "matvec":
		return ComputeRoute{Kind: "matrix_vector", Operation: "matrix-vector-rows", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_MatrixVector"}, true
	case "hc_dft", "dft_bins", "dft-bins":
		return ComputeRoute{Kind: "dft", Operation: "dft-bins", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_DFT"}, true
	case "hc_convolve2d", "convolve2d", "convolve2d-rows":
		return ComputeRoute{Kind: "convolve2d", Operation: "convolve2d-rows", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_Convolve2D"}, true
	case "hc_integratemidpoint", "integrate_midpoint", "integrate-midpoint":
		return ComputeRoute{Kind: "integrate", Operation: "integrate-midpoint", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_IntegrateMidpoint"}, true
	case "hc_sha256blocks", "sha256_blocks", "sha256-blocks":
		return ComputeRoute{Kind: "sha256_blocks", Operation: "sha256-blocks", CanonicalLibrary: "homecluster.dll", CanonicalFunction: "HC_SHA256Blocks"}, true
	default:
		return ComputeRoute{}, false
	}
}

func buildComputeProxy(proxyOut string, exports []Export, wrapped []WrappedExport, forwardBase string) error {
	tmpDir, err := os.MkdirTemp("", "hc_compute_wrap_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cpp := filepath.Join(tmpDir, "hc_compute_wrap.cpp")
	obj := filepath.Join(tmpDir, "hc_compute_wrap.obj")
	def := filepath.Join(tmpDir, "hc_compute_wrap.def")

	var c strings.Builder
	c.WriteString(`#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string>
#include <vector>

static const char* kDefaultHCPipe = "\\\\.\\pipe\\HomeCluster-Compute";
static volatile const char kHCSafeMarker[] = "HC_SAFE_FORWARD_HC_PIPE_V2";

static std::string hcPipeName() {
    char overrideName[256] = {};
    DWORD n = GetEnvironmentVariableA("HC_COMPUTE_PIPE", overrideName, (DWORD)sizeof(overrideName));
    if (n > 0 && n < sizeof(overrideName)) {
        const std::string candidate(overrideName, (size_t)n);
        // Test isolation may use another HomeCluster-owned pipe name. Refuse
        // arbitrary pipe namespaces so production routing cannot be redirected
        // outside the HomeCluster contract by an unrelated environment value.
        if (candidate.rfind("\\\\.\\pipe\\HomeCluster-", 0) == 0) return candidate;
    }
    return std::string(kDefaultHCPipe);
}

static std::string hcEscapeJSON(const std::string& in) {
    std::string out;
    out.reserve(in.size() + 16);
    char hex[7];
    for (unsigned char ch : in) {
        switch (ch) {
        case '\\': out += "\\\\"; break;
        case '"':  out += "\\\""; break;
        case '\b': out += "\\b"; break;
        case '\f': out += "\\f"; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        case '\t': out += "\\t"; break;
        default:
            if (ch < 0x20) {
                _snprintf_s(hex, sizeof(hex), _TRUNCATE, "\\u%04x", (unsigned)ch);
                out += hex;
            } else out.push_back((char)ch);
        }
    }
    return out;
}

static bool hcWriteAll(HANDLE pipe, const char* data, size_t size) {
    size_t offset = 0;
    while (offset < size) {
        DWORD written = 0;
        DWORD chunk = (DWORD)((size - offset) > (1u << 20) ? (1u << 20) : (size - offset));
        if (!WriteFile(pipe, data + offset, chunk, &written, NULL) || written == 0) return false;
        offset += written;
    }
    return true;
}

static bool hcReadLine(HANDLE pipe, std::string& out) {
    out.clear();
    char buffer[4096];
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(pipe, buffer, sizeof(buffer), &got, NULL) || got == 0) return false;
        for (DWORD i = 0; i < got; ++i) {
            if (buffer[i] == '\n') return true;
            out.push_back(buffer[i]);
            if (out.size() > 4u * 1024u * 1024u) return false;
        }
    }
}

static bool hcSuccess(const std::string& response) {
    return response.find("\"success\":true") != std::string::npos ||
           response.find("\"success\": true") != std::string::npos;
}

static std::string hcVectorJSON(const double* v, unsigned long long n) {
    std::string out;
    out.push_back('[');
    char number[64];
    for (unsigned long long i = 0; i < n; ++i) {
        if (i) out.push_back(',');
        int len = _snprintf_s(number, sizeof(number), _TRUNCATE, "%.17g", v[i]);
        if (len > 0) out.append(number, (size_t)len);
        else out += "0";
    }
    out.push_back(']');
    return out;
}

static bool hcRequest(const char* libraryName, const char* functionName,
                      const std::vector<std::string>& args, std::string& response) {
    if (!libraryName || !*libraryName || !functionName || !*functionName) return false;
    const std::string pipeName = hcPipeName();
    (void)WaitNamedPipeA(pipeName.c_str(), 2000);
    HANDLE pipe = CreateFileA(pipeName.c_str(), GENERIC_READ | GENERIC_WRITE, 0, NULL, OPEN_EXISTING, 0, NULL);
    if (pipe == INVALID_HANDLE_VALUE) return false;

    char correlation[128];
    _snprintf_s(correlation, sizeof(correlation), _TRUNCATE,
                "proxy-wrapper-%lu-%llu",
                (unsigned long)GetCurrentProcessId(),
                (unsigned long long)GetTickCount64());

    std::string req =
        "{\"correlationId\":\"" + hcEscapeJSON(correlation) +
        "\",\"library\":\"" + hcEscapeJSON(libraryName) +
        "\",\"function\":\"" + hcEscapeJSON(functionName) +
        "\",\"arguments\":[";
    for (size_t i = 0; i < args.size(); ++i) {
        if (i) req.push_back(',');
        req += "\"" + hcEscapeJSON(args[i]) + "\"";
    }
    req += "]}\n";
    if (req.size() > 4u * 1024u * 1024u) {
        CloseHandle(pipe);
        return false;
    }

    bool ok = hcWriteAll(pipe, req.data(), req.size()) && hcReadLine(pipe, response);
    CloseHandle(pipe);
    return ok && hcSuccess(response);
}

static bool hcExtractVector(const std::string& response, double* out, unsigned long long n) {
    if ((!out && n) || !hcSuccess(response)) return false;
    size_t p = response.find("\"result\"");
    if (p == std::string::npos) return false;
    p = response.find('[', p);
    if (p == std::string::npos) return false;
    ++p;
    for (unsigned long long i = 0; i < n; ++i) {
        while (p < response.size() &&
              (response[p] == ' ' || response[p] == '\t' || response[p] == '\r' ||
               response[p] == '\n' || response[p] == ',')) ++p;
        if (p >= response.size() || response[p] == ']') return false;
        char* end = NULL;
        double x = strtod(response.c_str() + p, &end);
        if (!end || end == response.c_str() + p) return false;
        out[i] = x;
        p = (size_t)(end - response.c_str());
    }
    return true;
}

static bool hcExtractScalar(const std::string& response, double* value) {
    if (!value || !hcSuccess(response)) return false;
    size_t p = response.find("\"result\"");
    if (p == std::string::npos) return false;
    p = response.find(':', p);
    if (p == std::string::npos) return false;
    ++p;
    while (p < response.size() &&
          (response[p] == ' ' || response[p] == '\t' || response[p] == '\r' || response[p] == '\n')) ++p;
    char* end = NULL;
    double x = strtod(response.c_str() + p, &end);
    if (!end || end == response.c_str() + p) return false;
    *value = x;
    return true;
}

static std::string hcNumber(double v) {
    char buf[64];
    int n = _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%.17g", v);
    return n > 0 ? std::string(buf, (size_t)n) : std::string("0");
}

static std::string hcU64(unsigned long long v) {
    char buf[64];
    int n = _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%llu", v);
    return n > 0 ? std::string(buf, (size_t)n) : std::string("0");
}

static std::string hcBase64(const unsigned char* data, unsigned long long len) {
    static const char table[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::string out;
    out.reserve((size_t)(((len + 2) / 3) * 4));
    for (unsigned long long i = 0; i < len; i += 3) {
        unsigned v = ((unsigned)data[i]) << 16;
        bool b1 = i + 1 < len, b2 = i + 2 < len;
        if (b1) v |= ((unsigned)data[i + 1]) << 8;
        if (b2) v |= (unsigned)data[i + 2];
        out.push_back(table[(v >> 18) & 63]);
        out.push_back(table[(v >> 12) & 63]);
        out.push_back(b1 ? table[(v >> 6) & 63] : '=');
        out.push_back(b2 ? table[v & 63] : '=');
    }
    return out;
}

static bool hcExtractStringArray(const std::string& response, char* out,
                                 unsigned long long stride, unsigned long long expected) {
    if (!out || stride < 2 || !hcSuccess(response)) return false;
    size_t p = response.find("\"result\"");
    if (p == std::string::npos) return false;
    p = response.find('[', p);
    if (p == std::string::npos) return false;
    ++p;
    for (unsigned long long i = 0; i < expected; ++i) {
        while (p < response.size() &&
              (response[p] == ' ' || response[p] == '\t' || response[p] == '\r' ||
               response[p] == '\n' || response[p] == ',')) ++p;
        if (p >= response.size() || response[p] != '"') return false;
        ++p;
        std::string value;
        bool esc = false;
        for (; p < response.size(); ++p) {
            char ch = response[p];
            if (esc) { value.push_back(ch); esc = false; continue; }
            if (ch == '\\') { esc = true; continue; }
            if (ch == '"') { ++p; break; }
            value.push_back(ch);
        }
        if (value.size() + 1 > stride) return false;
        memcpy(out + i * stride, value.data(), value.size());
        out[i * stride + value.size()] = '\0';
    }
    return true;
}

static bool hcRequestPayload(const char* libraryName, const char* functionName,
                             const std::string& payload, std::string& response) {
    std::vector<std::string> args;
    args.push_back(payload);
    return hcRequest(libraryName, functionName, args, response);
}

BOOL WINAPI DllMain(HINSTANCE, DWORD reason, LPVOID) {
	return reason != DLL_PROCESS_ATTACH || kHCSafeMarker[0] != '\0';
}
`)

	for _, w := range wrapped {
		lib := escapeC(w.Route.CanonicalLibrary)
		fn := escapeC(w.Route.CanonicalFunction)
		switch w.Route.Kind {
		case "vector_add":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* a, const double* b, double* out, unsigned long long n) {\n")
			c.WriteString("    if (!a || !b || !out || n == 0 || n > 65536ULL) return -1002;\n")
			c.WriteString("    std::vector<std::string> args; args.push_back(hcVectorJSON(a,n)); args.push_back(hcVectorJSON(b,n));\n")
			c.WriteString("    std::string response; return (hcRequest(\"" + lib + "\", \"" + fn + "\", args, response) && hcExtractVector(response,out,n)) ? 0 : -1001;\n")
			c.WriteString("}\n")
		case "vector_sub", "vector_mul":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* a, const double* b, double* out, unsigned long long n) {\n")
			c.WriteString("    if (!a || !b || !out || n == 0 || n > 65536ULL) return -1002;\n")
			c.WriteString("    std::string payload = \"{\\\"a\\\":\" + hcVectorJSON(a,n) + \",\\\"b\\\":\" + hcVectorJSON(b,n) + \"}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractVector(response,out,n)) ? 0 : -1001;\n")
			c.WriteString("}\n")
		case "vector_dot":
			c.WriteString("\nextern \"C\" double __cdecl " + w.HelperName + "(const double* a, const double* b, unsigned long long n) {\n")
			c.WriteString("    if (!a || !b || n == 0 || n > 65536ULL) { SetLastError(ERROR_INVALID_PARAMETER); return 0.0; }\n")
			c.WriteString("    std::vector<std::string> args; args.push_back(hcVectorJSON(a,n)); args.push_back(hcVectorJSON(b,n));\n")
			c.WriteString("    std::string response; double value=0.0; if (hcRequest(\"" + lib + "\", \"" + fn + "\", args, response) && hcExtractScalar(response,&value)) return value;\n")
			c.WriteString("    SetLastError(ERROR_SERVICE_NOT_ACTIVE); return 0.0;\n")
			c.WriteString("}\n")
		case "vector_scale":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* values, double scalar, double* out, unsigned long long n) {\n")
			c.WriteString("    if (!values || !out || n == 0 || n > 65536ULL) return -1002;\n")
			c.WriteString("    char scalarText[64]; _snprintf_s(scalarText, sizeof(scalarText), _TRUNCATE, \"%.17g\", scalar);\n")
			c.WriteString("    std::vector<std::string> args; args.push_back(hcVectorJSON(values,n)); args.push_back(scalarText);\n")
			c.WriteString("    std::string response; return (hcRequest(\"" + lib + "\", \"" + fn + "\", args, response) && hcExtractVector(response,out,n)) ? 0 : -1001;\n")
			c.WriteString("}\n")
		case "reduce":
			c.WriteString("\nextern \"C\" double __cdecl " + w.HelperName + "(const double* values, unsigned long long n) {\n")
			c.WriteString("    if (!values || n == 0 || n > 65536ULL) { SetLastError(ERROR_INVALID_PARAMETER); return 0.0; }\n")
			c.WriteString("    std::string payload = \"{\\\"values\\\":\" + hcVectorJSON(values,n) + \"}\"; std::string response; double value=0.0;\n")
			c.WriteString("    if (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractScalar(response,&value)) return value;\n")
			c.WriteString("    SetLastError(ERROR_SERVICE_NOT_ACTIVE); return 0.0;\n}\n")
		case "matrix_multiply":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* A, const double* B, double* C, unsigned long long aRows, unsigned long long aCols, unsigned long long bCols) {\n")
			c.WriteString("    if (!A || !B || !C || !aRows || !aCols || !bCols || aRows > 262144ULL || aCols > 262144ULL || bCols > 262144ULL) return -1002;\n")
			c.WriteString("    if (aRows > 262144ULL/aCols || aCols > 262144ULL/bCols || aRows > 262144ULL/bCols) return -1002;\n")
			c.WriteString("    unsigned long long an=aRows*aCols, bn=aCols*bCols, cn=aRows*bCols; std::string payload=\"{\\\"a\\\":\"+hcVectorJSON(A,an)+\",\\\"b\\\":\"+hcVectorJSON(B,bn)+\",\\\"a_rows\\\":\"+hcU64(aRows)+\",\\\"a_cols\\\":\"+hcU64(aCols)+\",\\\"b_cols\\\":\"+hcU64(bCols)+\"}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractVector(response,C,cn)) ? 0 : -1001;\n}\n")
		case "matrix_vector":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* matrix, const double* vector, double* out, unsigned long long rows, unsigned long long cols) {\n")
			c.WriteString("    if (!matrix || !vector || !out || !rows || !cols || rows > 262144ULL || cols > 262144ULL || rows > 262144ULL/cols) return -1002;\n")
			c.WriteString("    std::string payload=\"{\\\"matrix\\\":\"+hcVectorJSON(matrix,rows*cols)+\",\\\"vector\\\":\"+hcVectorJSON(vector,cols)+\",\\\"rows\\\":\"+hcU64(rows)+\",\\\"cols\\\":\"+hcU64(cols)+\"}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractVector(response,out,rows)) ? 0 : -1001;\n}\n")
		case "dft":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* values, double* outRealImag, unsigned long long n) {\n")
			c.WriteString("    if (!values || !outRealImag || !n || n > 65536ULL) return -1002; std::string payload=\"{\\\"values\\\":\"+hcVectorJSON(values,n)+\"}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractVector(response,outRealImag,n*2ULL)) ? 0 : -1001;\n}\n")
		case "convolve2d":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const double* input, unsigned long long width, unsigned long long height, const double* kernel, unsigned long long kw, unsigned long long kh, double* out) {\n")
			c.WriteString("    if (!input || !kernel || !out || !width || !height || !kw || !kh || width > 262144ULL || height > 262144ULL || width > 262144ULL/height || kw > 4096ULL || kh > 4096ULL || kw > 4096ULL/kh) return -1002;\n")
			c.WriteString("    std::string payload=\"{\\\"input\\\":\"+hcVectorJSON(input,width*height)+\",\\\"kernel\\\":\"+hcVectorJSON(kernel,kw*kh)+\",\\\"width\\\":\"+hcU64(width)+\",\\\"height\\\":\"+hcU64(height)+\",\\\"kernel_width\\\":\"+hcU64(kw)+\",\\\"kernel_height\\\":\"+hcU64(kh)+\"}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractVector(response,out,width*height)) ? 0 : -1001;\n}\n")
		case "integrate":
			c.WriteString("\nextern \"C\" double __cdecl " + w.HelperName + "(const char* functionName, double lower, double upper, unsigned long long steps, const double* coefficients, unsigned long long coefficientCount) {\n")
			c.WriteString("    if (!functionName || !steps || steps > 5000000ULL) { SetLastError(ERROR_INVALID_PARAMETER); return 0.0; } std::string f(functionName); if (f != \"sin\" && f != \"cos\" && f != \"exp-neg-square\" && f != \"polynomial\") { SetLastError(ERROR_INVALID_PARAMETER); return 0.0; }\n")
			c.WriteString("    std::string payload=\"{\\\"function\\\":\\\"\"+hcEscapeJSON(f)+\"\\\",\\\"lower\\\":\"+hcNumber(lower)+\",\\\"upper\\\":\"+hcNumber(upper)+\",\\\"steps\\\":\"+hcU64(steps); if(f==\"polynomial\"){if(!coefficients||coefficientCount==0||coefficientCount>65536ULL){SetLastError(ERROR_INVALID_PARAMETER);return 0.0;}payload+=\",\\\"coefficients\\\":\"+hcVectorJSON(coefficients,coefficientCount);}payload+=\"}\"; std::string response; double value=0.0;\n")
			c.WriteString("    if (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractScalar(response,&value)) return value; SetLastError(ERROR_SERVICE_NOT_ACTIVE); return 0.0;\n}\n")
		case "sha256_blocks":
			c.WriteString("\nextern \"C\" int __cdecl " + w.HelperName + "(const unsigned char* data, const unsigned long long* offsets, const unsigned long long* lengths, unsigned long long blockCount, char* outHex, unsigned long long outStride) {\n")
			c.WriteString("    if (!data || !offsets || !lengths || !outHex || !blockCount || blockCount > 65536ULL || outStride < 65ULL) return -1002; std::string payload=\"{\\\"blocks\\\":[\"; unsigned long long total=0;\n")
			c.WriteString("    for(unsigned long long i=0;i<blockCount;++i){if(lengths[i]>524288ULL||total>524288ULL-lengths[i])return -1002;total+=lengths[i];if(i)payload.push_back(',');payload+=\"\\\"\"+hcBase64(data+offsets[i],lengths[i])+\"\\\"\";}payload+=\"]}\"; std::string response;\n")
			c.WriteString("    return (hcRequestPayload(\"" + lib + "\", \"" + fn + "\", payload, response) && hcExtractStringArray(response,outHex,outStride,blockCount)) ? 0 : -1001;\n}\n")
		default:
			return fmt.Errorf("internal unsupported route %q", w.Route.Kind)
		}
	}

	if err := os.WriteFile(cpp, []byte(c.String()), 0644); err != nil {
		return err
	}
	wrappedByOrdinal := make(map[int]WrappedExport, len(wrapped))
	for _, w := range wrapped {
		wrappedByOrdinal[w.Export.Ordinal] = w
	}
	var d strings.Builder
	d.WriteString("LIBRARY \"")
	d.WriteString(strings.TrimSuffix(filepath.Base(proxyOut), filepath.Ext(proxyOut)))
	d.WriteString("\"\nEXPORTS\n")
	for _, e := range exports {
		if strings.ContainsAny(e.Name, "\r\n=\"") {
			return fmt.Errorf("unsupported export name %q", e.Name)
		}
		target := forwardBase + "." + e.Name
		if w, ok := wrappedByOrdinal[e.Ordinal]; ok {
			target = w.HelperName
		}
		fmt.Fprintf(&d, "  %s=%s @%d\n", e.Name, target, e.Ordinal)
	}
	if err := os.WriteFile(def, []byte(d.String()), 0644); err != nil {
		return err
	}
	if outb, err := runMSVC(clPath, "/nologo", "/c", "/O2", "/MD", "/EHsc", "/Fo"+obj, cpp); err != nil {
		return fmt.Errorf("compute helper compile failed: %v\n%s", err, string(outb))
	}
	if outb, err := runMSVC(linkPath, "/nologo", "/DLL", "/OUT:"+proxyOut, "/DEF:"+def, obj, "kernel32.lib"); err != nil {
		return fmt.Errorf("compute proxy link failed: %v\n%s", err, string(outb))
	}

	got, err := readNamedExports(proxyOut)
	if err != nil {
		return fmt.Errorf("verify compute proxy: %w", err)
	}
	have := make(map[string]bool)
	for _, e := range got {
		have[e.Name] = true
	}
	for _, w := range wrapped {
		if !have[w.Export.Name] {
			return fmt.Errorf("verify compute proxy missing export %s", w.Export.Name)
		}
	}
	return nil
}

type peNamedExport struct {
	Name string
	Slot uint16
}

func buildNamedForwarderPE(exports []Export, dllName, forwardBase string, helperTargets map[int]string) ([]byte, error) {
	if len(exports) == 0 {
		return nil, errors.New("no named exports")
	}
	minOrd, maxOrd := exports[0].Ordinal, exports[0].Ordinal
	for _, e := range exports {
		if e.Ordinal <= 0 {
			return nil, fmt.Errorf("invalid export ordinal %d", e.Ordinal)
		}
		if e.Ordinal < minOrd {
			minOrd = e.Ordinal
		}
		if e.Ordinal > maxOrd {
			maxOrd = e.Ordinal
		}
	}
	count64 := int64(maxOrd) - int64(minOrd) + 1
	if count64 <= 0 || count64 > 65536 {
		return nil, fmt.Errorf("unsupported export ordinal span: %d", count64)
	}
	count := uint32(count64)
	active := make([]bool, count)
	named := make([]peNamedExport, 0, len(exports))
	for _, e := range exports {
		slot := e.Ordinal - minOrd
		if slot < 0 || slot > 0xFFFF {
			return nil, fmt.Errorf("export ordinal outside supported span: %d", e.Ordinal)
		}
		active[slot] = true
		named = append(named, peNamedExport{Name: e.Name, Slot: uint16(slot)})
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Name < named[j].Name })

	const sectionRVA = uint32(0x1000)
	const fileAlign = uint32(0x200)
	const sectionAlign = uint32(0x1000)
	const headersSize = uint32(0x200)
	align32 := func(v, a uint32) uint32 { return (v + a - 1) &^ (a - 1) }

	offDir := uint32(0)
	offFuncs := uint32(40)
	offNames := offFuncs + count*4
	offOrds := offNames + uint32(len(named))*4
	offStrings := align32(offOrds+uint32(len(named))*2, 4)

	var sb bytes.Buffer
	dllNameOff := offStrings + uint32(sb.Len())
	sb.WriteString(dllName)
	sb.WriteByte(0)

	nameOffsets := make([]uint32, len(named))
	for i, n := range named {
		nameOffsets[i] = offStrings + uint32(sb.Len())
		sb.WriteString(n.Name)
		sb.WriteByte(0)
	}

	forwardOffsets := make([]uint32, count)
	for i := uint32(0); i < count; i++ {
		if !active[i] {
			continue
		}
		ord := minOrd + int(i)
		forwardOffsets[i] = offStrings + uint32(sb.Len())
		if target, ok := helperTargets[ord]; ok && target != "" {
			sb.WriteString(target)
		} else {
			sb.WriteString(forwardBase)
			sb.WriteString(".#")
			sb.WriteString(strconv.Itoa(ord))
		}
		sb.WriteByte(0)
	}
	// Preserve the existing marker and keep the original forward module visible
	// even for the degenerate case where every active slot is wrapped.
	sb.WriteString("HC_SAFE_FORWARD_HC_PIPE_V2")
	sb.WriteByte(0)
	sb.WriteString(forwardBase)
	sb.WriteByte(0)

	virtualSize := offStrings + uint32(sb.Len())
	rawSize := align32(virtualSize, fileAlign)
	edata := make([]byte, rawSize)
	put32 := func(off, v uint32) { binary.LittleEndian.PutUint32(edata[off:off+4], v) }
	put16 := func(off uint32, v uint16) { binary.LittleEndian.PutUint16(edata[off:off+2], v) }
	now := uint32(time.Now().Unix())
	put32(offDir+4, now)
	put32(offDir+12, sectionRVA+dllNameOff)
	put32(offDir+16, uint32(minOrd))
	put32(offDir+20, count)
	put32(offDir+24, uint32(len(named)))
	put32(offDir+28, sectionRVA+offFuncs)
	put32(offDir+32, sectionRVA+offNames)
	put32(offDir+36, sectionRVA+offOrds)
	for i := uint32(0); i < count; i++ {
		if forwardOffsets[i] != 0 {
			put32(offFuncs+i*4, sectionRVA+forwardOffsets[i])
		}
	}
	for i, n := range named {
		put32(offNames+uint32(i)*4, sectionRVA+nameOffsets[i])
		put16(offOrds+uint32(i)*2, n.Slot)
	}
	copy(edata[offStrings:], sb.Bytes())

	imageSize := align32(sectionRVA+virtualSize, sectionAlign)
	out := make([]byte, headersSize+rawSize)
	binary.LittleEndian.PutUint16(out[0:2], 0x5A4D)
	binary.LittleEndian.PutUint32(out[0x3c:0x40], 0x80)
	copy(out[0x40:], []byte("This program cannot be run in DOS mode.\r\r\n$"))
	pe := uint32(0x80)
	binary.LittleEndian.PutUint32(out[pe:pe+4], 0x00004550)
	coff := pe + 4
	binary.LittleEndian.PutUint16(out[coff:coff+2], 0x8664)
	binary.LittleEndian.PutUint16(out[coff+2:coff+4], 1)
	binary.LittleEndian.PutUint32(out[coff+4:coff+8], now)
	binary.LittleEndian.PutUint16(out[coff+16:coff+18], 0xF0)
	binary.LittleEndian.PutUint16(out[coff+18:coff+20], 0x2022)
	opt := coff + 20
	binary.LittleEndian.PutUint16(out[opt:opt+2], 0x20B)
	out[opt+2] = 1
	binary.LittleEndian.PutUint32(out[opt+8:opt+12], rawSize)
	binary.LittleEndian.PutUint64(out[opt+24:opt+32], 0x180000000)
	binary.LittleEndian.PutUint32(out[opt+32:opt+36], sectionAlign)
	binary.LittleEndian.PutUint32(out[opt+36:opt+40], fileAlign)
	binary.LittleEndian.PutUint16(out[opt+40:opt+42], 6)
	binary.LittleEndian.PutUint16(out[opt+48:opt+50], 6)
	binary.LittleEndian.PutUint32(out[opt+56:opt+60], imageSize)
	binary.LittleEndian.PutUint32(out[opt+60:opt+64], headersSize)
	binary.LittleEndian.PutUint16(out[opt+68:opt+70], 3)
	binary.LittleEndian.PutUint16(out[opt+70:opt+72], 0x8160)
	binary.LittleEndian.PutUint64(out[opt+72:opt+80], 1<<20)
	binary.LittleEndian.PutUint64(out[opt+80:opt+88], 1<<12)
	binary.LittleEndian.PutUint64(out[opt+88:opt+96], 1<<20)
	binary.LittleEndian.PutUint64(out[opt+96:opt+104], 1<<12)
	binary.LittleEndian.PutUint32(out[opt+108:opt+112], 16)
	binary.LittleEndian.PutUint32(out[opt+112:opt+116], sectionRVA)
	binary.LittleEndian.PutUint32(out[opt+116:opt+120], virtualSize)
	sec := opt + 0xF0
	copy(out[sec:sec+8], []byte(".edata\x00\x00"))
	binary.LittleEndian.PutUint32(out[sec+8:sec+12], virtualSize)
	binary.LittleEndian.PutUint32(out[sec+12:sec+16], sectionRVA)
	binary.LittleEndian.PutUint32(out[sec+16:sec+20], rawSize)
	binary.LittleEndian.PutUint32(out[sec+20:sec+24], headersSize)
	binary.LittleEndian.PutUint32(out[sec+36:sec+40], 0x40000040)
	copy(out[headersSize:], edata)
	return out, nil
}

func readNamedExports(path string) ([]Export, error) {
	out, err := runMSVC(dumpbinPath, "/nologo", "/exports", path)
	if err != nil {
		return nil, fmt.Errorf("dumpbin failed: %w\n%s", err, string(out))
	}
	var result []Export
	in := false
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "ordinal") && strings.Contains(lower, "hint") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.EqualFold(line, "Summary") {
			break
		}

		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		ord, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}

		name := ""
		for j := 2; j < len(f); j++ {
			tok := f[j]
			if tok == "=" || strings.HasPrefix(tok, "(") {
				continue
			}
			if isHexRVA(tok) {
				continue
			}
			name = tok
			break
		}
		if name == "" || strings.HasPrefix(name, "[") {
			continue
		}
		result = append(result, Export{Ordinal: ord, Name: name})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ordinal < result[j].Ordinal })
	return result, nil
}

func isHexRVA(s string) bool {
	if len(s) != 8 && len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func findMSVC() error {
	pf86 := os.Getenv("ProgramFiles(x86)")
	pf := os.Getenv("ProgramFiles")
	candidates := []string{
		filepath.Join(pf86, "Microsoft Visual Studio", "Installer", "vswhere.exe"),
		filepath.Join(pf, "Microsoft Visual Studio", "Installer", "vswhere.exe"),
	}
	var vswhere string
	for _, p := range candidates {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				vswhere = p
				break
			}
		}
	}
	if vswhere == "" {
		return fmt.Errorf("vswhere.exe not found")
	}

	out, err := exec.Command(vswhere,
		"-latest", "-products", "*",
		"-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
		"-property", "installationPath").Output()
	if err != nil {
		return err
	}
	vs := strings.TrimSpace(string(out))
	if vs == "" {
		return fmt.Errorf("Visual Studio C++ Build Tools not found")
	}

	versions, _ := filepath.Glob(filepath.Join(vs, "VC", "Tools", "MSVC", "*"))
	if len(versions) == 0 {
		return fmt.Errorf("MSVC not found")
	}
	sort.Strings(versions)
	msvc := versions[len(versions)-1]
	bin := filepath.Join(msvc, "bin", "Hostx64", "x64")
	clPath = filepath.Join(bin, "cl.exe")
	linkPath = filepath.Join(bin, "link.exe")
	dumpbinPath = filepath.Join(bin, "dumpbin.exe")
	for _, p := range []string{clPath, linkPath, dumpbinPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing MSVC tool: %s", p)
		}
	}

	sdkRoot := filepath.Join(pf86, "Windows Kits", "10")
	sdkVers, _ := filepath.Glob(filepath.Join(sdkRoot, "Include", "*"))
	if len(sdkVers) == 0 {
		return fmt.Errorf("Windows SDK not found")
	}
	sort.Strings(sdkVers)
	sdk := filepath.Base(sdkVers[len(sdkVers)-1])

	includePath = []string{
		filepath.Join(msvc, "include"),
		filepath.Join(sdkRoot, "Include", sdk, "ucrt"),
		filepath.Join(sdkRoot, "Include", sdk, "shared"),
		filepath.Join(sdkRoot, "Include", sdk, "um"),
	}
	libPath = []string{
		filepath.Join(msvc, "lib", "x64"),
		filepath.Join(sdkRoot, "Lib", sdk, "ucrt", "x64"),
		filepath.Join(sdkRoot, "Lib", sdk, "um", "x64"),
	}
	return nil
}

func runMSVC(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	env := os.Environ()
	env = append(env, "INCLUDE="+strings.Join(includePath, ";"))
	env = append(env, "LIB="+strings.Join(libPath, ";"))
	cmd.Env = env
	return cmd.CombinedOutput()
}

func defTokenOK(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		// Whitespace and these characters are DEF syntax delimiters.
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '=' || r == '"' || r == '#' || r == ';' {
			return false
		}
	}
	return true
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "proxy"
	}
	return b.String()
}

func escapeC(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
