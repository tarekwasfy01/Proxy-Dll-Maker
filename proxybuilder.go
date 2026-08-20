// proxybuilder_cli_v8.go
// HomeCluster-compatible Windows x64 proxy builder.
// CLI:
//   proxybuilder.exe build --dll "C:\path\foo.dll" --out "C:\path\foo_proxy.dll" --forward-module "foo_o.dll"
//
// Safe forwarder builder: unknown exports are ABI-neutral PE forwarders to
// <forward-module>. Only explicitly signature-known functions may use wrappers.
// This prevents arbitrary native DLL exports from losing register/stack args.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

func main() {
	if len(os.Args) < 2 || !strings.EqualFold(os.Args[1], "build") {
		usage()
		os.Exit(2)
	}

	var dllPath, outPath, forwardModule string
	for i := 2; i < len(os.Args); i++ {
		switch strings.ToLower(os.Args[i]) {
		case "--dll":
			if i+1 < len(os.Args) {
				i++
				dllPath = os.Args[i]
			}
		case "--out":
			if i+1 < len(os.Args) {
				i++
				outPath = os.Args[i]
			}
		case "--forward-module":
			if i+1 < len(os.Args) {
				i++
				forwardModule = os.Args[i]
			}
		}
	}

	dllPath = filepath.Clean(strings.TrimSpace(dllPath))
	outPath = filepath.Clean(strings.TrimSpace(outPath))
	forwardModule = strings.TrimSpace(forwardModule)

	if dllPath == "." || dllPath == "" || outPath == "." || outPath == "" {
		usage()
		os.Exit(2)
	}
	if forwardModule == "" {
		base := strings.TrimSuffix(filepath.Base(dllPath), filepath.Ext(dllPath))
		forwardModule = base + "_o" + filepath.Ext(dllPath)
	}
	// Module name only. Never embed an arbitrary path in generated code.
	forwardModule = filepath.Base(forwardModule)

	if err := findMSVC(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	if err := buildProxy(dllPath, outPath, forwardModule); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}

	fmt.Printf("BUILT %s\n", outPath)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: proxybuilder.exe build --dll <source.dll> --out <proxy.dll> [--forward-module <name_o.dll>]`)
}

func buildProxy(dll, out, forwardModule string) error {
	exports, err := readNamedExports(dll)
	if err != nil {
		return err
	}
	if len(exports) == 0 {
		return fmt.Errorf("no named exports")
	}

	base := strings.TrimSuffix(filepath.Base(dll), filepath.Ext(dll))
	tmpDir, err := os.MkdirTemp("", "hcwrap_"+base+"_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cpp := filepath.Join(tmpDir, base+"_proxy.cpp")
	def := filepath.Join(tmpDir, base+"_proxy.def")
	obj := filepath.Join(tmpDir, base+"_proxy.obj")

	var c strings.Builder
	c.WriteString(`#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <stdint.h>

typedef uintptr_t (__cdecl *HCUniversalCallFn)(
    const char* libraryName,
    const char* functionName,
    int argc,
    const char* const* argv
);

static HMODULE g_core = NULL;
static HMODULE g_orig = NULL;
static volatile char kHomeClusterSafeProxyMarker[] = "HC_SAFE_FORWARD_V8";

static HCUniversalCallFn getHC(void) {
    if (!g_core) g_core = LoadLibraryW(L"universal_proxy.dll");
    if (!g_core) return NULL;
    return (HCUniversalCallFn)GetProcAddress(g_core, "HC_UniversalCall");
}

static FARPROC getOrig(const char* name) {
    if (!g_orig) g_orig = LoadLibraryW(L"` + escapeWideLiteral(forwardModule) + `");
    if (!g_orig) return NULL;
    return GetProcAddress(g_orig, name);
}

BOOL WINAPI DllMain(HINSTANCE, DWORD, LPVOID) {
    volatile char marker = kHomeClusterSafeProxyMarker[0];
    (void)marker;
    return TRUE;
}
`)

	// Only this zero-argument COM lifetime probe has an exact signature in this
	// generic builder. Every other export remains a direct PE forwarder so its
	// ABI is preserved perfectly. Structured compute functions use dedicated
	// adapters/bridges instead of guessed native signatures.
	wrap := map[string]bool{}
	if strings.EqualFold(filepath.Base(dll), "abovelockapphost.dll") {
		for _, e := range exports {
			if e.Name == "DllCanUnloadNow" {
				wrap[e.Name] = true
			}
		}
	}

	for _, e := range exports {
		if !wrap[e.Name] {
			continue
		}
		safe := cIdent(e.Name)
		c.WriteString("\nextern \"C\" HRESULT WINAPI HCW_" + safe + "(void) {\n")
		c.WriteString("    HCUniversalCallFn hc = getHC();\n")
		c.WriteString("    if (hc) {\n")
		c.WriteString("        uintptr_t r = hc(\"" + escapeC(filepath.Base(dll)) + "\", \"" + escapeC(e.Name) + "\", 0, NULL);\n")
		c.WriteString("        if (r != 0) return (HRESULT)r;\n")
		c.WriteString("    }\n")
		c.WriteString("    typedef HRESULT (WINAPI *OrigFn)(void);\n")
		c.WriteString("    OrigFn fn = (OrigFn)getOrig(\"" + escapeC(e.Name) + "\");\n")
		c.WriteString("    if (!fn) return E_FAIL;\n")
		c.WriteString("    return fn();\n")
		c.WriteString("}\n")
	}

	if err := os.WriteFile(cpp, []byte(c.String()), 0644); err != nil {
		return err
	}

	forwardBase := strings.TrimSuffix(filepath.Base(forwardModule), filepath.Ext(forwardModule))
	var d strings.Builder
	d.WriteString("LIBRARY " + base + "\nEXPORTS\n")
	for _, e := range exports {
		// Forward unknown exports by ORDINAL rather than by export name. This is
		// robust for decorated/unusual names and data exports because LINK never
		// has to interpret the source export name as a local unresolved symbol.
		lhs := defQuote(e.Name)
		if wrap[e.Name] {
			d.WriteString(fmt.Sprintf("    %s=HCW_%s @%d\n", lhs, cIdent(e.Name), e.Ordinal))
		} else {
			d.WriteString(fmt.Sprintf("    %s=%s.#%d @%d\n", lhs, forwardBase, e.Ordinal, e.Ordinal))
		}
	}
	if err := os.WriteFile(def, []byte(d.String()), 0644); err != nil {
		return err
	}

	if outb, err := runMSVC(clPath, "/nologo", "/c", "/O2", "/MD", "/EHsc", "/Fo"+obj, cpp); err != nil {
		return fmt.Errorf("compile failed: %v\n%s", err, string(outb))
	}
	if outb, err := runMSVC(linkPath, "/nologo", "/DLL", "/OUT:"+out, "/DEF:"+def, obj, "kernel32.lib"); err != nil {
		return fmt.Errorf("link failed: %v\n%s", err, string(outb))
	}

	got, err := readNamedExports(out)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if len(got) != len(exports) {
		return fmt.Errorf("verify export count mismatch: source=%d proxy=%d", len(exports), len(got))
	}

	// Verify that the requested _o module is present in the PE forwarder table.
	// Pure forwarder DLLs do not need a LoadLibraryW wide literal, so verify the
	// ASCII module base used by the export-forwarder strings instead.
	bin, err := os.ReadFile(out)
	if err != nil {
		return fmt.Errorf("verify output read: %w", err)
	}
	forwardBaseBytes := []byte(strings.TrimSuffix(filepath.Base(forwardModule), filepath.Ext(forwardModule)))
	if !bytes.Contains(bytes.ToLower(bin), bytes.ToLower(forwardBaseBytes)) {
		return fmt.Errorf("verify failed: forward module %q not embedded in proxy", forwardModule)
	}

	if !bytes.Contains(bin, []byte("HC_SAFE_FORWARD_V8")) {
		return fmt.Errorf("verify failed: safe proxy marker missing")
	}

	fmt.Printf("SAFE HC_SAFE_FORWARD_V8\n")
	fmt.Printf("FORWARD %s\n", forwardModule)
	return nil
}

func readNamedExports(path string) ([]Export, error) {
	out, err := runMSVC(dumpbinPath, "/nologo", "/exports", path)
	if err != nil {
		return nil, fmt.Errorf("dumpbin: %w\n%s", err, string(out))
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
		if len(f) < 4 {
			continue
		}

		ord, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}

		// dumpbin formats normal exports as:
		//   ordinal hint RVA      name
		// and forwarded exports as:
		//   ordinal hint          name = TARGET.name
		// Therefore find the first plausible symbol token after ordinal/hint
		// instead of assuming a fixed column.
		name := ""
		for j := 2; j < len(f); j++ {
			tok := f[j]
			if tok == "=" || strings.HasPrefix(tok, "(") {
				continue
			}
			// Skip an 8/16 digit hexadecimal RVA column.
			isHex := true
			for _, r := range tok {
				if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F') || (r >= 'a' && r <= 'f')) {
					isHex = false
					break
				}
			}
			if isHex && (len(tok) == 8 || len(tok) == 16) {
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

func findMSVC() error {
	vswhere := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
	if _, err := os.Stat(vswhere); err != nil {
		vswhere = filepath.Join(os.Getenv("ProgramFiles"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
	}
	if _, err := os.Stat(vswhere); err != nil {
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
		return fmt.Errorf("Visual Studio Build Tools not found")
	}

	versions, err := filepath.Glob(filepath.Join(vs, "VC", "Tools", "MSVC", "*"))
	if err != nil || len(versions) == 0 {
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

	sdkRoot := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Windows Kits", "10")
	sdkVers, err := filepath.Glob(filepath.Join(sdkRoot, "Include", "*"))
	if err != nil || len(sdkVers) == 0 {
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

func defQuote(name string) string {
	// Quoted DEF names preserve decorated/special export names verbatim.
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

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
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func escapeWideLiteral(s string) string {
	return escapeC(filepath.Base(s))
}
