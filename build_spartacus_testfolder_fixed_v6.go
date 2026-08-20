// build_spartacus_testfolder_fixed_v6.go
// Baut ABI-korrekte Proxy-DLLs mit Spartacus + direkten MSVC-Tools.
// Keine Developer Command Prompt nötig – findet MSVC selbst.
// Zielordner: C:\Users\tarek\Desktop\Neuer Ordner (5)\shims

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Nur DLLs aus diesem Testordner werden verarbeitet.
	testDllDir   = `C:\Users\tarek\Desktop\Neuer Ordner (5)\test_dlls`
	outputDir    = `C:\Users\tarek\Desktop\Neuer Ordner (5)\shims`
	spartacusExe = `C:\Users\tarek\Desktop\Neuer Ordner (5)\SpartacusDummy.exe`
	maxWorkers   = 1
)

var (
	successCount int
	failCount    int
	skipCount    int
	mu           sync.Mutex
)

// MSVC-Pfade (werden beim Start gefüllt)
var (
	clPath       string
	ml64Path     string
	linkPath     string
	dumpbinPath  string
	includePaths []string
	libPaths     []string
)

func main() {
	startTime := time.Now()

	fmt.Println("=== HomeCluster Proxy Builder (Standalone MSVC) ===")
	fmt.Println()

	// Spartacus prüfen
	if _, err := os.Stat(spartacusExe); err != nil {
		fmt.Printf("ERROR: Spartacus not found at %s\n", spartacusExe)
		return
	}
	fmt.Printf("Spartacus: %s\n", spartacusExe)

	// MSVC-Tools und Pfade finden
	if err := findMSVCTools(); err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	fmt.Printf("cl.exe:   %s\n", clPath)
	fmt.Printf("ml64.exe: %s\n", ml64Path)
	fmt.Printf("link.exe: %s\n", linkPath)
	fmt.Printf("dumpbin:  %s\n", dumpbinPath)
	fmt.Printf("INCLUDE:  %s\n", strings.Join(includePaths, ";"))
	fmt.Printf("LIB:      %s\n", strings.Join(libPaths, ";"))

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Printf("ERROR: Failed to create output dir: %v\n", err)
		return
	}

	// Nur den lokalen Test-DLL-Ordner scannen, nicht System32.
	if err := os.MkdirAll(testDllDir, 0755); err != nil {
		fmt.Printf("ERROR: Failed to create test DLL dir: %v\n", err)
		return
	}
	files, err := filepath.Glob(filepath.Join(testDllDir, "*.dll"))
	if err != nil {
		fmt.Printf("ERROR: Glob failed: %v\n", err)
		return
	}
	fmt.Printf("Test DLL folder: %s\n", testDllDir)
	fmt.Printf("Found %d test DLLs\n\n", len(files))
	if len(files) == 0 {
		fmt.Println("No DLLs found. Put your test DLLs into the test_dlls folder and run again.")
		return
	}

	// NUR 5 ZUM TESTEN – später entfernen!
	//if len(files) > 5 {
	//	files = files[:5]
	//}

	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for _, dll := range files {
		wg.Add(1)
		go func(dllPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processDLL(dllPath)
		}(dll)
	}
	wg.Wait()

	fmt.Printf("\n=== DONE ===\n")
	fmt.Printf("Time: %s\n", time.Since(startTime))
	fmt.Printf("Success: %d\n", successCount)
	fmt.Printf("Failed: %d\n", failCount)
	fmt.Printf("Skipped: %d\n", skipCount)
	fmt.Printf("Proxy DLLs: %s\n", outputDir)
}

func findMSVCTools() error {
	// 1. vswhere finden
	vswhere := filepath.Join(
		os.Getenv("ProgramFiles(x86)"),
		"Microsoft Visual Studio",
		"Installer",
		"vswhere.exe",
	)
	if _, err := os.Stat(vswhere); err != nil {
		vswhere = filepath.Join(
			os.Getenv("ProgramFiles"),
			"Microsoft Visual Studio",
			"Installer",
			"vswhere.exe",
		)
		if _, err := os.Stat(vswhere); err != nil {
			return fmt.Errorf("vswhere.exe not found")
		}
	}

	// 2. VS-Pfad holen
	out, err := exec.Command(
		vswhere,
		"-latest",
		"-products", "*",
		"-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
		"-property", "installationPath",
	).Output()
	if err != nil {
		return fmt.Errorf("vswhere failed: %w", err)
	}
	vsPath := strings.TrimSpace(string(out))
	if vsPath == "" {
		return fmt.Errorf("Visual Studio not found")
	}
	fmt.Printf("Visual Studio: %s\n", vsPath)

	// 3. MSVC-Tools finden
	msvcTools := filepath.Join(vsPath, "VC", "Tools", "MSVC")
	versions, err := filepath.Glob(filepath.Join(msvcTools, "*"))
	if err != nil || len(versions) == 0 {
		return fmt.Errorf("no MSVC tools found under %s", msvcTools)
	}
	latestVersion := versions[len(versions)-1]
	toolsBin := filepath.Join(latestVersion, "bin", "Hostx64", "x64")

	clPath = filepath.Join(toolsBin, "cl.exe")
	if _, err := os.Stat(clPath); err != nil {
		return fmt.Errorf("cl.exe not found at %s", clPath)
	}
	ml64Path = filepath.Join(toolsBin, "ml64.exe")
	if _, err := os.Stat(ml64Path); err != nil {
		return fmt.Errorf("ml64.exe not found at %s", ml64Path)
	}
	linkPath = filepath.Join(toolsBin, "link.exe")
	if _, err := os.Stat(linkPath); err != nil {
		// Fallback: im Windows SDK suchen
		sdkLink := os.Getenv("ProgramFiles(x86)") + `\Windows Kits\10\bin\10.0.22621.0\x64\link.exe`
		if _, err := os.Stat(sdkLink); err == nil {
			linkPath = sdkLink
		} else {
			return fmt.Errorf("link.exe not found")
		}
	}
	dumpbinPath = filepath.Join(toolsBin, "dumpbin.exe")
	if _, err := os.Stat(dumpbinPath); err != nil {
		sdkDumpbin := os.Getenv("ProgramFiles(x86)") + `\Windows Kits\10\bin\10.0.22621.0\x64\dumpbin.exe`
		if _, err := os.Stat(sdkDumpbin); err == nil {
			dumpbinPath = sdkDumpbin
		} else {
			return fmt.Errorf("dumpbin.exe not found")
		}
	}

	// 4. INCLUDE-Pfade finden
	msvcInclude := filepath.Join(latestVersion, "include")
	sdkRoot := os.Getenv("ProgramFiles(x86)") + `\Windows Kits\10`
	sdkInclude := filepath.Join(sdkRoot, "Include")
	sdkVersions, err := filepath.Glob(filepath.Join(sdkInclude, "*"))
	if err != nil || len(sdkVersions) == 0 {
		return fmt.Errorf("Windows SDK not found")
	}
	latestSdk := sdkVersions[len(sdkVersions)-1]
	includePaths = []string{
		msvcInclude,
		filepath.Join(latestSdk, "ucrt"),
		filepath.Join(latestSdk, "shared"),
		filepath.Join(latestSdk, "um"),
		filepath.Join(latestSdk, "winrt"),
	}

	// 5. LIB-Pfade finden
	msvcLib := filepath.Join(latestVersion, "lib", "x64")
	sdkLib := filepath.Join(sdkRoot, "Lib", filepath.Base(latestSdk))
	libPaths = []string{
		msvcLib,
		filepath.Join(sdkLib, "ucrt", "x64"),
		filepath.Join(sdkLib, "um", "x64"),
	}

	return nil
}

func runWithEnv(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	// Umgebung setzen
	env := os.Environ()
	env = append(env, "INCLUDE="+strings.Join(includePaths, ";"))
	env = append(env, "LIB="+strings.Join(libPaths, ";"))
	cmd.Env = env
	return cmd.CombinedOutput()
}

func processDLL(dllPath string) {
	name := filepath.Base(dllPath)
	base := strings.TrimSuffix(name, ".dll")

	if strings.Contains(name, "{") || strings.Contains(name, "}") {
		mu.Lock()
		skipCount++
		mu.Unlock()
		fmt.Printf("Skipping %s (GUID)\n", name)
		return
	}

	// 1. Spartacus: Proxy-Projekt generieren
	solutionDir := filepath.Join(os.TempDir(), "spartacus_"+base)
	if err := os.RemoveAll(solutionDir); err != nil {
		fmt.Printf("  ERROR: Cannot clear temp dir: %v\n", err)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}
	if err := os.MkdirAll(solutionDir, 0755); err != nil {
		fmt.Printf("  ERROR: Cannot create temp dir: %v\n", err)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}

	cmd := exec.Command(spartacusExe,
		"--mode", "proxy",
		"--dll", dllPath,
		"--solution", solutionDir,
		"--overwrite",
		"--verbose",
	)
	if _, err := cmd.CombinedOutput(); err != nil {
		mu.Lock()
		skipCount++
		mu.Unlock()
		fmt.Printf("Skipping %s (Spartacus: no exports)\n", name)
		return
	}
	fmt.Printf("Processing %s\n", name)

	// 2. .cpp-Datei finden
	cppPath := filepath.Join(solutionDir, base+".cpp")
	if _, err := os.Stat(cppPath); err != nil {
		cppPath = filepath.Join(solutionDir, "dllmain.cpp")
		if _, err := os.Stat(cppPath); err != nil {
			mu.Lock()
			skipCount++
			mu.Unlock()
			fmt.Printf("Skipping %s (no .cpp)\n", name)
			return
		}
	}

	// TEST ONLY: Alle von Spartacus eingebetteten Quell-DLL-Pfade auf
	// reine Modulnamen umschreiben. Das betrifft sowohl Export-Forwarder
	// als auch LoadLibrary(...). DLLs außerhalb von testDllDir werden abgelehnt.
	if err := patchTestGeneratedSource(cppPath, dllPath); err != nil {
		fmt.Printf("  ERROR: Failed to remove test-folder paths: %v\n", err)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}

	// 3. Die von Spartacus erzeugte DEF-Datei verwenden.
	// Spartacus exportiert nicht zwingend mit __declspec(dllexport) in dllmain.cpp;
	// deshalb war die alte Suche in der C++-Datei falsch und meldete "No exports found".
	defFile := filepath.Join(solutionDir, base+".def")
	if _, err := os.Stat(defFile); err != nil {
		fmt.Printf("  ERROR: Spartacus DEF not found: %s\n", defFile)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}
	defExportCount, err := countDefExports(defFile)
	if err != nil {
		fmt.Printf("  ERROR: Failed to read DEF: %v\n", err)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}
	sourceExportCount, err := countPEExports(dllPath)
	if err != nil {
		fmt.Printf("  WARNING: Could not count source DLL exports: %v\n", err)
	} else {
		fmt.Printf("  Source DLL exports: %d\n", sourceExportCount)
	}
	fmt.Printf("  Spartacus DEF explicit mappings: %d\n", defExportCount)
	if defExportCount == 0 {
		fmt.Printf("  Note: 0 here can be normal; Spartacus can emit passthrough exports via linker pragmas in dllmain.cpp.\n")
	}

	// 4. C++ kompilieren (unveränderte Spartacus-Ausgabe)
	cObj := filepath.Join(solutionDir, base+".obj")
	if out, err := runWithEnv(clPath, "/nologo", "/c", "/O2", "/MD", "/DUNICODE", "/D_UNICODE", "/EHsc", "/Fo"+cObj, cppPath); err != nil {
		fmt.Printf("  C compile error: %v\n%s\n", err, out)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}

	// 7. DLL linken (mit MSVC-Umgebung)
	proxyPath := filepath.Join(outputDir, base+"_proxy.dll")
	if out, err := runWithEnv(linkPath,
		"/nologo", "/DLL", "/OUT:"+proxyPath, "/DEF:"+defFile,
		cObj, "kernel32.lib",
	); err != nil {
		fmt.Printf("  Link error: %v\n%s\n", err, out)
		mu.Lock()
		failCount++
		mu.Unlock()
		return
	}

	if builtExportCount, err := countPEExports(proxyPath); err != nil {
		fmt.Printf("  WARNING: Could not verify built DLL exports: %v\n", err)
	} else {
		fmt.Printf("  Built DLL exports: %d\n", builtExportCount)
		if sourceExportCount > 0 && builtExportCount == 0 {
			fmt.Printf("  WARNING: Source DLL had exports, but built DLL has none.\n")
		}
	}
	fmt.Printf("  Proxy saved to: %s\n", proxyPath)
	mu.Lock()
	successCount++
	mu.Unlock()
}

func patchTestGeneratedSource(cppPath string, dllPath string) error {
	absRoot, err := filepath.Abs(testDllDir)
	if err != nil {
		return err
	}
	absDLL, err := filepath.Abs(dllPath)
	if err != nil {
		return err
	}

	rel, err := filepath.Rel(absRoot, absDLL)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to rewrite paths outside test folder")
	}

	data, err := os.ReadFile(cppPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	moduleFile := filepath.Base(absDLL)
	moduleNoExt := strings.TrimSuffix(moduleFile, filepath.Ext(moduleFile))
	changedForwarders := 0
	changedLoadLibrary := 0

	for i, line := range lines {
		updated := line

		// 1) Linker-Forwarder:
		//    /export:Name=C:\\...\test_dlls\foo.Name,@123
		// -> /export:Name=foo.Name,@123
		if strings.Contains(updated, "#pragma comment(linker") && strings.Contains(updated, "/export:") {
			exportPos := strings.Index(updated, "/export:")
			eqPosRel := -1
			if exportPos >= 0 {
				eqPosRel = strings.Index(updated[exportPos:], "=")
			}
			if exportPos >= 0 && eqPosRel >= 0 {
				eqPos := exportPos + eqPosRel
				endRel := strings.Index(updated[eqPos+1:], ",@")
				if endRel >= 0 {
					targetEnd := eqPos + 1 + endRel
					target := updated[eqPos+1 : targetEnd]
					dot := strings.LastIndex(target, ".")
					if dot > 0 && dot < len(target)-1 {
						functionPart := target[dot+1:]
						newTarget := moduleNoExt + "." + functionPart
						if newTarget != target {
							updated = updated[:eqPos+1] + newTarget + updated[targetEnd:]
							changedForwarders++
						}
					}
				}
			}
		}

		// 2) Spartacus' LoadLibrary-Zeile enthält ebenfalls den vollen Quellpfad.
		//    Diese wird für den Testordner auf den Dateinamen reduziert:
		//    LoadLibrary(L"C:\\...\test_dlls\foo.dll") -> LoadLibrary(L"foo.dll")
		for _, prefix := range []string{`LoadLibrary(L"`, `LoadLibraryW(L"`, `LoadLibraryA("`} {
			pos := strings.Index(updated, prefix)
			if pos < 0 {
				continue
			}
			valueStart := pos + len(prefix)
			end := strings.Index(updated[valueStart:], `"`)
			if end < 0 {
				continue
			}
			valueEnd := valueStart + end
			rawValue := updated[valueStart:valueEnd]
			normalized := strings.ReplaceAll(rawValue, `\\`, `\`)
			normalized = filepath.Clean(normalized)

			if strings.EqualFold(normalized, absDLL) {
				updated = updated[:valueStart] + moduleFile + updated[valueEnd:]
				changedLoadLibrary++
			}
		}

		lines[i] = updated
	}

	content := strings.Join(lines, "\n")

	// Zusätzliche Absicherung: Falls Spartacus den Testordner noch an einer
	// anderen Stelle in den generierten C++-Text geschrieben hat, entfernen wir
	// exakt diesen bekannten lokalen Präfix. Das bleibt auf testDllDir begrenzt.
	rootVariants := []string{
		absRoot + string(os.PathSeparator),
		strings.ReplaceAll(absRoot+string(os.PathSeparator), `\`, `\\`),
	}
	for _, root := range rootVariants {
		for {
			lowerContent := strings.ToLower(content)
			lowerRoot := strings.ToLower(root)
			idx := strings.Index(lowerContent, lowerRoot)
			if idx < 0 {
				break
			}
			content = content[:idx] + content[idx+len(root):]
		}
	}

	// Harte Prüfung: der Testordner darf danach weder normal noch C++-escaped
	// in dllmain.cpp vorkommen.
	lower := strings.ToLower(content)
	for _, forbidden := range []string{
		strings.ToLower(absRoot),
		strings.ToLower(strings.ReplaceAll(absRoot, `\`, `\\`)),
	} {
		if forbidden != "" && strings.Contains(lower, forbidden) {
			return fmt.Errorf("test-folder path is still present in generated C++")
		}
	}

	if err := os.WriteFile(cppPath, []byte(content), 0644); err != nil {
		return err
	}

	fmt.Printf("  Forwarders rewritten without paths: %d\n", changedForwarders)
	fmt.Printf("  LoadLibrary paths rewritten: %d\n", changedLoadLibrary)
	return nil
}

func countPEExports(path string) (int, error) {
	out, err := runWithEnv(dumpbinPath, "/nologo", "/exports", path)
	if err != nil {
		return 0, fmt.Errorf("dumpbin failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	count := 0
	inTable := false
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "ordinal") && strings.Contains(lower, "hint") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if strings.EqualFold(line, "Summary") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err == nil {
			count++
		}
	}
	return count, nil
}

func countDefExports(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	count := 0
	inExports := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.EqualFold(line, "EXPORTS") {
			inExports = true
			continue
		}
		if !inExports {
			continue
		}
		// Jede nichtleere Zeile unter EXPORTS ist ein Export-Eintrag.
		count++
	}
	return count, nil
}
