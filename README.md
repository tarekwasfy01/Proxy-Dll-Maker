# Proxy DLL Maker (Go + MSVC)

This Go script automates the generation and compilation of x64 proxy DLL projects with [Spartacus](https://github.com/Accenture/Spartacus) and the Microsoft C/C++ build tools.

It scans a folder for DLL files, asks Spartacus to generate a proxy project for each DLL, compiles the generated C++ source with MSVC, links the DLL, and writes the resulting proxy DLLs to an output folder.

> **Important:** Use this only with DLLs and software you own or are authorized to test.

---

## Requirements

### 1. Windows

The script is intended for **64-bit Windows**.

### 2. Go

Install Go:

https://go.dev/dl/

Check the installation:

```powershell
go version
```

### 3. Microsoft Visual Studio / Build Tools

Install either:

- Visual Studio with **Desktop development with C++**, or
- Visual Studio Build Tools with the C++ toolchain.

Download:

https://visualstudio.microsoft.com/downloads/

The installation must include:

- MSVC x64 compiler (`cl.exe`)
- MASM x64 assembler (`ml64.exe`)
- Microsoft linker (`link.exe`)
- `dumpbin.exe`
- Windows SDK
- x64 MSVC libraries and headers

The script uses `vswhere.exe` to locate the newest installed Visual Studio C++ toolchain automatically, so a Developer Command Prompt is not required.

### 4. Spartacus

Project:

https://github.com/Accenture/Spartacus

Download/build Spartacus according to its GitHub instructions.

The script calls Spartacus in proxy mode using arguments equivalent to:

```text
--mode proxy
--dll <path-to-dll>
--solution <temporary-folder>
--overwrite
--verbose
```

---

## Configuration

Open the Go file and edit the values near the top:

```go
const (
    testDllDir   = `C:\path\to\your\dlls`
    outputDir    = `C:\path\to\output`
    spartacusExe = `C:\path\to\Spartacus.exe`
    maxWorkers   = 1
)
```

### DLL folder

Set `testDllDir` to the folder that contains the DLL files you want to process.

Example:

```go
testDllDir = `C:\Users\YourName\Desktop\DLLs`
```

Put your DLL files directly inside that folder:

```text
DLLs\
├─ example1.dll
├─ example2.dll
└─ example3.dll
```

The current script scans:

```text
*.dll
```

inside that folder only.

It does **not** recursively scan subfolders.

> The current version does not show a graphical folder picker. The DLL folder is selected by changing `testDllDir` in the source file.

### Output folder

Set `outputDir` to the folder where the generated proxy DLLs should be saved.

Example:

```go
outputDir = `C:\Users\YourName\Desktop\shims`
```

The folder is created automatically if it does not already exist.

Generated files are named like:

```text
example1_proxy.dll
example2_proxy.dll
example3_proxy.dll
```

### Spartacus executable

Set `spartacusExe` to your Spartacus executable.

Example:

```go
spartacusExe = `C:\Tools\Spartacus\Spartacus.exe`
```

If you use a local test/dummy build of Spartacus, point the setting to that executable instead.

### Worker count

```go
maxWorkers = 1
```

Controls how many DLLs are processed at the same time.

For initial testing, `1` is recommended because the output is easier to read and failures are easier to diagnose.

---

## Example folder layout

```text
C:\
└─ ProxyBuilder\
   ├─ build_spartacus_testfolder_fixed_v6.go
   ├─ Spartacus.exe
   ├─ dlls\
   │  ├─ example1.dll
   │  └─ example2.dll
   └─ shims\
```

Example configuration:

```go
const (
    testDllDir   = `C:\ProxyBuilder\dlls`
    outputDir    = `C:\ProxyBuilder\shims`
    spartacusExe = `C:\ProxyBuilder\Spartacus.exe`
    maxWorkers   = 1
)
```

---

## Running the script

Open PowerShell and run:

```powershell
go run ".\build_spartacus_testfolder_fixed_v6.go"
```

Or use the full path:

```powershell
go run "C:\ProxyBuilder\build_spartacus_testfolder_fixed_v6.go"
```

---

## What happens during a run

At startup the script:

1. checks whether the configured Spartacus executable exists;
2. locates Visual Studio with `vswhere.exe`;
3. locates:
   - `cl.exe`
   - `ml64.exe`
   - `link.exe`
   - `dumpbin.exe`;
4. discovers the newest installed Windows SDK;
5. builds the required `INCLUDE` and `LIB` paths;
6. scans the configured DLL folder for `*.dll`;
7. processes each DLL with Spartacus;
8. compiles the generated C++ source;
9. links the proxy DLL;
10. verifies the exported-function count with `dumpbin`;
11. saves the final DLL as `<original-name>_proxy.dll`.

Temporary Spartacus project files are created below the Windows temporary directory and are not written to the output folder.

---

## Example console output

```text
=== HomeCluster Proxy Builder (Standalone MSVC) ===

Spartacus: C:\ProxyBuilder\Spartacus.exe
Visual Studio: C:\Program Files\Microsoft Visual Studio\...
cl.exe:   ...
ml64.exe: ...
link.exe: ...
dumpbin:  ...

Test DLL folder: C:\ProxyBuilder\dlls
Found 2 test DLLs

Processing example1.dll
  Source DLL exports: 10
  Spartacus DEF explicit mappings: 0
  Built DLL exports: 10
  Proxy saved to: C:\ProxyBuilder\shims\example1_proxy.dll

=== DONE ===
Success: 1
Failed: 0
Skipped: 0
```

---

## Notes about exports

Spartacus may represent exports in more than one way.

Some exports can be present in the generated `.def` file, while pass-through exports can also be generated through linker pragmas in `dllmain.cpp`.

Because of this, a message such as:

```text
Spartacus DEF explicit mappings: 0
```

does **not automatically mean** that the DLL has no exports.

The script also runs `dumpbin /exports` on both the source DLL and the built DLL to verify the resulting export table.

---

## Path rewriting used by this script

For DLLs inside the configured DLL folder, the script removes the absolute test-folder path from the C++ source generated by Spartacus.

For example, a generated forwarder such as:

```text
C:\ProxyBuilder\dlls\example.SomeFunction
```

is rewritten to:

```text
example.SomeFunction
```

A generated call such as:

```cpp
LoadLibrary(L"C:\\ProxyBuilder\\dlls\\example.dll")
```

is rewritten to:

```cpp
LoadLibrary(L"example.dll")
```

The script refuses to perform this rewrite for DLLs outside the configured DLL folder.

---

## Troubleshooting

### `vswhere.exe not found`

Install Visual Studio or Visual Studio Build Tools from:

https://visualstudio.microsoft.com/downloads/

### `cl.exe not found`

Install the **Desktop development with C++** workload.

### `ml64.exe not found`

Make sure the MSVC x64 toolchain is installed.

### `Windows SDK not found`

Open Visual Studio Installer and add a Windows SDK component.

### `Spartacus not found`

Check the value of:

```go
spartacusExe
```

and make sure it points to the actual executable.

### `No DLLs found`

Check:

```go
testDllDir
```

and make sure the DLL files are directly inside that folder.

### `Skipping ... (Spartacus: no exports)`

Spartacus could not generate a usable proxy project for that DLL, or the DLL does not expose exports supported by the selected proxy-generation path.

### Link or compile errors

Read the complete MSVC error printed by the script.

Typical causes are:

- missing Windows SDK components;
- unsupported generated C++ code;
- architecture mismatch;
- missing libraries;
- a DLL export that requires special handling.

---

## Build the Go script as an EXE

Instead of running it with `go run`, you can build it once:

```powershell
go build -o SpartacusProxyBuilder.exe ".\build_spartacus_testfolder_fixed_v6.go"
```

Then start:

```powershell
.\SpartacusProxyBuilder.exe
```

---

## Architecture

This script currently uses the x64 MSVC toolchain:

```text
Hostx64\x64
```

Therefore it is intended for **x64 DLLs**.

If you need x86 or ARM64 support, the toolchain selection and build settings must be changed accordingly.

---

## Credits

- [Spartacus – Accenture](https://github.com/Accenture/Spartacus)
- [Go](https://go.dev/)
- [Microsoft Visual Studio / Build Tools](https://visualstudio.microsoft.com/downloads/)

---

## License

Use the license that applies to your own repository and code.

Spartacus is a separate third-party project and remains subject to its own license.
