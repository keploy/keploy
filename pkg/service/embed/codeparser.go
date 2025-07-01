package embed

// #cgo LDFLAGS: -ldl
// #include <dlfcn.h>
// #include <stdlib.h>
//
// // Forward declare TSLanguage so we can use it in function pointer types
// typedef struct TSLanguage TSLanguage;
//
// // Define a function pointer type that matches tree_sitter_xxxx()
// typedef TSLanguage* (*LanguageSymbolFunc)();
//
// TSLanguage* call_language_func(void* func_ptr) {
//     if (func_ptr == NULL) return NULL;
//     LanguageSymbolFunc lang_func = (LanguageSymbolFunc)func_ptr;
//     return lang_func();
// }
//
// void* my_dlopen(const char* filename) {
//     // RTLD_GLOBAL might be needed if grammars have dependencies on each other,
//     // or if the tree-sitter core library itself is a separate .so being implicitly loaded.
//     // For standalone grammars, RTLD_LAZY is often sufficient.
//     return dlopen(filename, RTLD_LAZY | RTLD_GLOBAL);
// }
//
// void* my_dlsym(void* handle, const char* symbol) {
//     return dlsym(handle, symbol);
// }
//
// int my_dlclose(void* handle) {
//     if (handle == NULL) return -1; // Or some other error indication
//     return dlclose(handle);
// }
import "C" // CGo import

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

var (
	CacheDir = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Could not get user home directory: %v", err)
		}
		return filepath.Join(home, ".code_parser_cache_go")
	}()

	languageExtensionMap = map[string]string{
		"py": "python",
		"js": "javascript",
		"go": "go",
	}

	dlHandlesMutex sync.Mutex
)

// PointOfInterest represents a node and its type label
type PointOfInterest struct {
	Node  *sitter.Node
	Label string
}

// CodeParser holds the tree-sitter languages
type CodeParser struct {
	languageNames []string
	languages     map[string]*sitter.Language
	langHandles   map[string]unsafe.Pointer
}

// If no extensions are provided, no parsers are initialized by default.
func NewCodeParser(fileExtensions ...string) (*CodeParser, error) {
	cp := &CodeParser{
		languages:   make(map[string]*sitter.Language),
		langHandles: make(map[string]unsafe.Pointer),
	}

	seenLangs := make(map[string]bool)
	for _, ext := range fileExtensions {
		langName, ok := languageExtensionMap[ext]
		if ok && !seenLangs[langName] {
			cp.languageNames = append(cp.languageNames, langName)
			seenLangs[langName] = true
		} else if !ok {
			log.Printf("WARN: Unsupported file extension '%s' provided to NewCodeParser, it will be ignored for parser installation.", ext)
		}
	}

	err := cp.installParsers()
	if err != nil {
		return nil, fmt.Errorf("failed to install parsers: %w", err)
	}
	return cp, nil
}

func (cp *CodeParser) installParsers() error {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if _, err := os.Stat(CacheDir); os.IsNotExist(err) {
		if err := os.MkdirAll(CacheDir, 0755); err != nil {
			return fmt.Errorf("failed to create cache directory %s: %w", CacheDir, err)
		}
	}
	buildDir := filepath.Join(CacheDir, "build")
	if _, err := os.Stat(buildDir); os.IsNotExist(err) {
		if err := os.MkdirAll(buildDir, 0755); err != nil {
			return fmt.Errorf("failed to create build directory %s: %w", buildDir, err)
		}
	}

	for _, language := range cp.languageNames {
		repoPath := filepath.Join(CacheDir, fmt.Sprintf("tree-sitter-%s", language))
		soPath := filepath.Join(buildDir, fmt.Sprintf("%s.so", language))

		if _, err := os.Stat(repoPath); os.IsNotExist(err) || !isRepoValid(repoPath) {
			if _, errRepoStat := os.Stat(repoPath); !os.IsNotExist(errRepoStat) {
				log.Printf("INFO: Repository for %s exists but seems invalid/incomplete or old. Attempting to update.", language)
				cmd := exec.Command("git", "-C", repoPath, "pull")
				output, errPull := cmd.CombinedOutput()
				if errPull != nil {
					log.Printf("WARN: Failed to update repository for %s, attempting re-clone. Error: %v. Output: %s", language, errPull, string(output))
					os.RemoveAll(repoPath)
					if errClone := cloneRepo(language, repoPath); errClone != nil {
						log.Printf("ERROR: Failed to clone repository for %s after update failure. Error: %v", language, errClone)
						continue
					}
				}
			} else {
				if err := cloneRepo(language, repoPath); err != nil {
					log.Printf("ERROR: Failed to clone repository for %s. Error: %v", language, err)
					continue
				}
			}
		}

		var srcFiles []string
		var includePaths []string

		repoSrcDir := filepath.Join(repoPath, "src")
		includePaths = append(includePaths, repoSrcDir)

		parserCFile := filepath.Join(repoSrcDir, "parser.c")
		srcFiles = append(srcFiles, parserCFile)

		scannerCFile := filepath.Join(repoSrcDir, "scanner.c")
		if _, err := os.Stat(scannerCFile); err == nil {
			srcFiles = append(srcFiles, scannerCFile)
		} else {
			scannerCCFile := filepath.Join(repoSrcDir, "scanner.cc")
			if _, err := os.Stat(scannerCCFile); err == nil {
				srcFiles = append(srcFiles, scannerCCFile)
			}
		}

		actualSrcFiles := []string{}
		parserFound := false
		for _, f := range srcFiles {
			if _, err := os.Stat(f); err == nil {
				actualSrcFiles = append(actualSrcFiles, f)
				if f == parserCFile {
					parserFound = true
				}
			}
		}

		if !parserFound {
			log.Printf("ERROR: Critical source file parser.c not found for language %s in %s. Skipping build.", language, repoSrcDir)
			continue
		}
		if len(actualSrcFiles) == 0 {
			log.Printf("ERROR: No source files confirmed for language %s. Repo path: %s", language, repoPath)
			continue
		}

		compiler := "gcc"
		compileArgs := []string{"-shared", "-o", soPath, "-fPIC"}
		for _, p := range includePaths {
			compileArgs = append(compileArgs, "-I"+p)
		}
		compileArgs = append(compileArgs, actualSrcFiles...)

		useCppLinker := false
		for _, f := range actualSrcFiles {
			if strings.HasSuffix(f, ".cc") {
				compiler = "g++"
				useCppLinker = true
				break
			}
		}
		if useCppLinker && compiler == "gcc" {
			compileArgs = append(compileArgs, "-lstdc++")
		}

		log.Printf("INFO: Compiling %s parser: %s %s", language, compiler, strings.Join(compileArgs, " "))
		cmd := exec.Command(compiler, compileArgs...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("ERROR: Failed to build parser for %s. Error: %v. Output: %s", language, err, string(output))
			log.Printf("ERROR: Repository path: %s", repoPath)
			log.Printf("ERROR: Build path: %s", soPath)
			log.Printf("ERROR: Source files considered: %v", actualSrcFiles)
			continue
		}
		log.Printf("INFO: Successfully built %s parser to %s", language, soPath)

		cSoPath := C.CString(soPath)
		handle := C.my_dlopen(cSoPath)
		C.free(unsafe.Pointer(cSoPath)) // Free after dlopen uses it
		if handle == nil {
			dlErr := C.dlerror()
			log.Printf("ERROR: Failed to dlopen %s: %s", soPath, C.GoString(dlErr))
			continue
		}
		cp.langHandles[language] = handle

		symbolName := "tree_sitter_" + strings.ReplaceAll(language, "-", "_")
		cSymbolName := C.CString(symbolName)
		funcPtr := C.my_dlsym(handle, cSymbolName)
		C.free(unsafe.Pointer(cSymbolName)) // Free after dlsym uses it

		if funcPtr == nil {
			dlErr := C.dlerror()
			log.Printf("ERROR: Failed to dlsym symbol %s in %s: %s", symbolName, soPath, C.GoString(dlErr))
			C.my_dlclose(handle) // Close if symbol not found
			cp.langHandles[language] = nil
			continue
		}

		actualLanguagePtr := C.call_language_func(funcPtr)
		if actualLanguagePtr == nil {
			log.Printf("ERROR: Calling language function for %s returned NULL", language)
			C.my_dlclose(handle)
			cp.langHandles[language] = nil
			continue
		}

		cp.languages[language] = sitter.NewLanguage(unsafe.Pointer(actualLanguagePtr))
		log.Printf("INFO: Successfully loaded %s parser from %s", language, soPath)
	}
	return nil
}

func cloneRepo(language, repoPath string) error {
	log.Printf("INFO: Cloning repository for %s into %s", language, repoPath)
	cloneURL := fmt.Sprintf("https://github.com/tree-sitter/tree-sitter-%s", language)
	cmd := exec.Command("git", "clone", cloneURL, repoPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed for %s: %w. Output: %s", cloneURL, err, string(output))
	}
	return nil
}

func isRepoValid(repoPath string) bool {
	parserCPath := filepath.Join(repoPath, "src", "parser.c")
	if _, err := os.Stat(parserCPath); os.IsNotExist(err) {
		log.Printf("DEBUG: Repo at %s potentially invalid, missing essential file %s", repoPath, parserCPath)
		return false
	}
	return true
}

func (cp *CodeParser) ParseCode(code string, fileExtension string) (*sitter.Node, error) {
	languageName, ok := languageExtensionMap[fileExtension]
	if !ok {
		return nil, fmt.Errorf("unsupported file type: %s. Supported extensions: py, js, go", fileExtension)
	}

	lang, ok := cp.languages[languageName]
	if !ok || lang == nil {
		return nil, fmt.Errorf("language parser for %s (extension: %s) not loaded. Ensure it was included in NewCodeParser and installed successfully", languageName, fileExtension)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, []byte(code))
	if err != nil {
		return nil, fmt.Errorf("failed to parse code (ext: %s): %w", fileExtension, err)
	}
	if tree == nil {
		return nil, fmt.Errorf("failed to parse the code (ext: %s), tree is nil", fileExtension)
	}
	return tree.RootNode(), nil
}

var nodeTypesOfInterestData = map[string]map[string]string{
	"py": {
		"import_statement":      "Import",
		"import_from_statement": "Import",
		"class_definition":      "Class",
		"function_definition":   "Function",
	},
	"js": {
		"import_statement":     "Import",
		"export_statement":     "Export",
		"class_declaration":    "Class",
		"function_declaration": "Function",
		"arrow_function":       "Arrow Function",
	},
	"go": {
		"import_declaration":   "Import",
		"function_declaration": "Function",
		"method_declaration":   "Method",
		"type_declaration":     "Type",
		"struct_type":          "Struct",
		"interface_type":       "Interface",
		"package_clause":       "Package",
	},
}

func (cp *CodeParser) getNodeTypesOfInterest(fileExtension string) (map[string]string, error) {
	types, ok := nodeTypesOfInterestData[fileExtension]
	if !ok {
		return nil, fmt.Errorf("unsupported file type for points of interest: %s. Supported: py, js, go", fileExtension)
	}
	return types, nil
}

// to identify points of interest in the code, such as imports, classes, functions.
func (cp *CodeParser) ExtractPointsOfInterest(node *sitter.Node, fileExtension string) ([]PointOfInterest, error) {
	interestMap, err := cp.getNodeTypesOfInterest(fileExtension)
	if err != nil {
		return nil, err
	}

	var points []PointOfInterest
	var recurse func(n *sitter.Node)
	recurse = func(n *sitter.Node) {
		if n == nil {
			return
		}
		nodeType := n.Type()
		if label, ok := interestMap[nodeType]; ok {
			points = append(points, PointOfInterest{Node: n, Label: label})
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			recurse(n.Child(i))
		}
	}
	recurse(node)
	return points, nil
}

var nodesForCommentsData = map[string]map[string]string{
	"py": {
		"comment":   "Comment",
		"decorator": "Decorator",
	},
	"js": {
		"comment":   "Comment",
		"decorator": "Decorator",
	},
	"go": {
		"comment": "Comment",
	},
}

func (cp *CodeParser) getNodesForComments(fileExtension string) (map[string]string, error) {
	types, ok := nodesForCommentsData[fileExtension]
	if !ok {
		return nil, fmt.Errorf("unsupported file type for comments: %s. Supported: py, js, go", fileExtension)
	}
	return types, nil
}

// extracts comments and decorators from the code.
func (cp *CodeParser) ExtractComments(node *sitter.Node, fileExtension string) ([]PointOfInterest, error) {
	commentMap, err := cp.getNodesForComments(fileExtension)
	if err != nil {
		return nil, err
	}

	var comments []PointOfInterest
	var recurse func(n *sitter.Node)
	recurse = func(n *sitter.Node) {
		if n == nil {
			return
		}
		nodeType := n.Type()
		if label, ok := commentMap[nodeType]; ok {
			comments = append(comments, PointOfInterest{Node: n, Label: label})
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			recurse(n.Child(i))
		}
	}
	recurse(node)
	return comments, nil
}

// will be called by chunker to get lines for points of interest.
func (cp *CodeParser) GetLinesForPointsOfInterest(code string, fileExtension string) (map[string][]int, error) {
	rootNode, err := cp.ParseCode(code, fileExtension)
	if err != nil {
		return nil, fmt.Errorf("parsing code for points of interest failed (ext: %s): %w", fileExtension, err)
	}

	points, err := cp.ExtractPointsOfInterest(rootNode, fileExtension)
	if err != nil {
		return nil, fmt.Errorf("extracting points of interest failed (ext: %s): %w", fileExtension, err)
	}

	lineNumbersWithLabels := make(map[string][]int)
	seenLinesForLabel := make(map[string]map[int]bool)

	for _, p := range points {
		startLine := int(p.Node.StartPoint().Row) // 0-indexed

		if _, ok := seenLinesForLabel[p.Label]; !ok {
			seenLinesForLabel[p.Label] = make(map[int]bool)
		}

		if !seenLinesForLabel[p.Label][startLine] {
			lineNumbersWithLabels[p.Label] = append(lineNumbersWithLabels[p.Label], startLine)
			seenLinesForLabel[p.Label][startLine] = true
		}
	}
	return lineNumbersWithLabels, nil
}

// will be called by chunker to get lines for comments and decorators.
func (cp *CodeParser) GetLinesForComments(code string, fileExtension string) (map[string][]int, error) {
	rootNode, err := cp.ParseCode(code, fileExtension)
	if err != nil {
		return nil, fmt.Errorf("parsing code for comments failed (ext: %s): %w", fileExtension, err)
	}

	comments, err := cp.ExtractComments(rootNode, fileExtension)
	if err != nil {
		return nil, fmt.Errorf("extracting comments failed (ext: %s): %w", fileExtension, err)
	}

	lineNumbersWithLabels := make(map[string][]int)
	seenLinesForLabel := make(map[string]map[int]bool)

	for _, c := range comments {
		startLine := int(c.Node.StartPoint().Row) // 0-indexed

		if _, ok := seenLinesForLabel[c.Label]; !ok {
			seenLinesForLabel[c.Label] = make(map[int]bool)
		}
		if !seenLinesForLabel[c.Label][startLine] {
			lineNumbersWithLabels[c.Label] = append(lineNumbersWithLabels[c.Label], startLine)
			seenLinesForLabel[c.Label][startLine] = true
		}
	}
	return lineNumbersWithLabels, nil
}

func (cp *CodeParser) PrintAllLineTypes(code string, fileExtension string) error {
	rootNode, err := cp.ParseCode(code, fileExtension)
	if err != nil {
		return fmt.Errorf("parsing code for printing line types failed (ext: %s): %w", fileExtension, err)
	}

	lineToNodeType := make(map[int][]string)
	cp.mapLineToNodeType(rootNode, lineToNodeType)

	codeLines := strings.Split(code, "\n")

	for lineNum := 1; lineNum <= len(codeLines); lineNum++ {
		nodeTypes, ok := lineToNodeType[lineNum]
		lineContent := ""
		if lineNum-1 < len(codeLines) {
			lineContent = codeLines[lineNum-1]
		}
		if ok {
			fmt.Printf("line %d: %s | Code: %s\n", lineNum, strings.Join(nodeTypes, ", "), lineContent)
		}
	}
	return nil
}

func (cp *CodeParser) mapLineToNodeType(node *sitter.Node, lineToNodeType map[int][]string) {
	if node == nil {
		return
	}
	startLine := int(node.StartPoint().Row) + 1 // 1-indexed for display

	lineToNodeType[startLine] = append(lineToNodeType[startLine], node.Type())

	for i := 0; i < int(node.ChildCount()); i++ {
		cp.mapLineToNodeType(node.Child(i), lineToNodeType)
	}
}

func (cp *CodeParser) Close() {
	dlHandlesMutex.Lock()
	defer dlHandlesMutex.Unlock()
	for lang, handle := range cp.langHandles {
		if handle != nil {
			C.my_dlclose(handle)
			log.Printf("INFO: Closed dl handle for %s", lang)
			cp.langHandles[lang] = nil
		}
	}
}
