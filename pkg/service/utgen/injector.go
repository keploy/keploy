package utgen

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

type Injector struct {
	logger   *zap.Logger
	language string
}

func NewInjectorBuilder(logger *zap.Logger, language string) *Injector {
	injectBuilder := &Injector{
		logger:   logger,
		language: language,
	}

	return injectBuilder
}
func (i *Injector) getLanguageVersion() (string, error) {
	switch strings.ToLower(i.language) {
	case "go":
		out, err := exec.Command("go", "version").Output()
		if err != nil {
			return "", fmt.Errorf("failed to get Go version: %w", err)
		}
		// Extract only the version part ("go1.22.0") if it's "go version go1.22.0 linux/amd64"
		parts := strings.Fields(string(out))
		if len(parts) >= 3 {
			return parts[2], nil
		}
		return "", fmt.Errorf("unexpected output format for Go version: %s", string(out))
	case "java":
		out, err := exec.Command("java", "-version").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to get Java version: %w", err)
		}
		// Use regex to extract the version number from the output
		re := regexp.MustCompile(`"(\d+\.\d+\.\d+)"`)
		if match := re.FindStringSubmatch(string(out)); len(match) > 1 {
			return match[1], nil
		}
		return "", fmt.Errorf("unexpected output format for Java version: %s", string(out))

	case "python":
		out, err := exec.Command("python", "--version").Output()
		if err != nil {
			out, err = exec.Command("python3", "--version").Output()
			if err != nil {
				return "", fmt.Errorf("failed to get Python version: %w", err)
			}
		}
		return strings.TrimSpace(string(out)), nil
	case "typescript", "javascript":
		out, err := exec.Command("node", "-v").Output()
		if err != nil {
			return "", fmt.Errorf("failed to get Node.js version: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	default:
		return "", fmt.Errorf("unsupported language: %s", i.language)
	}
}

func (i *Injector) libraryInstalled() (map[string]string, error) {
	switch strings.ToLower(i.language) {
	case "go":
		out, err := exec.Command("go", "list", "-m", "all").Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get Go dependencies: %w", err)
		}
		return i.extractGoDependencies(out), nil

	case "java":
		cmd := exec.Command("mvn", "dependency:list", "-DincludeScope=compile", "-Dstyle.color=never", "-B")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get Java dependencies: %w", err)
		}
		return i.extractJavaDependencies(out), nil

	case "python":
		cmd := exec.Command("pip", "freeze")
		out, err := cmd.Output()
		if err != nil {
			i.logger.Info("Error getting Python dependencies with `pip` command, trying `pip3` command")
			out, err = exec.Command("pip3", "freeze").Output()
			if err != nil {
				return nil, fmt.Errorf("failed to get Python dependencies: %w", err)
			}
		}
		return i.extractPythonDependencies(out), nil

	case "typescript", "javascript":
		cmd := exec.Command("sh", "-c", "npm list --depth=0")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to get JavaScript/TypeScript dependencies: %w", err)
		}
		return i.extractJSDependencies(out), nil

	default:
		return nil, fmt.Errorf("unsupported language: %s", i.language)
	}
}

// Extract Go dependencies as a map
func (i *Injector) extractGoDependencies(output []byte) map[string]string {
	lines := strings.Split(string(output), "\n")
	dependencies := make(map[string]string)
	for _, line := range lines {
		if len(line) > 0 {
			parts := strings.Split(line, " ")
			if len(parts) == 2 {
				dependencies[parts[0]] = parts[1] // Map package name to version
			}
		}
	}
	return dependencies
}

// Extract Python dependencies as a map
func (i *Injector) extractPythonDependencies(output []byte) map[string]string {
	lines := strings.Split(string(output), "\n")
	dependencies := make(map[string]string)
	for _, line := range lines {
		parts := strings.Split(line, "==")
		if len(parts) == 2 {
			dependencies[parts[0]] = parts[1] // Map package name to version
		}
	}
	return dependencies
}

// Extract Java dependencies as a map
func (i *Injector) extractJavaDependencies(output []byte) map[string]string {
	lines := strings.Split(string(output), "\n")
	dependencies := make(map[string]string)
	inDependencySection := false
	depRegex := regexp.MustCompile(`^\[INFO\]\s+[\+\|\\\-]{1,2}\s+([\w\.\-]+:[\w\.\-]+):jar:([\w\.\-]+):([\w\.\-]+)`)

	for _, line := range lines {
		cleanedLine := strings.TrimSpace(line)
		if strings.HasPrefix(cleanedLine, "[INFO]") {
			cleanedLine = "[INFO]" + strings.TrimSpace(cleanedLine[6:])
		}
		if strings.Contains(cleanedLine, "maven-dependency-plugin") && strings.Contains(cleanedLine, ":list") {
			inDependencySection = true
			continue
		}

		if inDependencySection && (strings.Contains(cleanedLine, "BUILD SUCCESS") || strings.Contains(cleanedLine, "---")) {
			inDependencySection = false
			continue
		}

		if inDependencySection && strings.HasPrefix(cleanedLine, "[INFO]") {
			matches := depRegex.FindStringSubmatch(cleanedLine)
			if len(matches) >= 4 {
				groupArtifact := matches[1]
				version := matches[3]
				dependencies[groupArtifact] = version // Map group:artifact to version
			}
		}
	}
	return dependencies
}

// Extract JS/TS dependencies as a map
func (i *Injector) extractJSDependencies(output []byte) map[string]string {
	lines := strings.Split(string(output), "\n")
	dependencies := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "├──")
		line = strings.TrimPrefix(line, "└──")
		line = strings.TrimPrefix(line, "│──")
		line = strings.TrimPrefix(line, "──")
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		lastAt := strings.LastIndex(line, "@")
		if lastAt > 0 {
			name := line[:lastAt]
			version := line[lastAt+1:]
			dependencies[name] = version // Map package name to version
		}
	}
	return dependencies
}
func (i *Injector) installLibraries2(libraryCommands string, installedPackages map[string]string) (map[string]map[string]string, error) {
	newInstalledPackagesMap := make(map[string]map[string]string)
	libraryCommands = strings.TrimSpace(libraryCommands)
	beforeVersionUpdate := true
	if libraryCommands == "" || libraryCommands == "\"\"" {
		i.logger.Info("No new libraries required.")
		return newInstalledPackagesMap, nil
	}
	commands := strings.Split(libraryCommands, "\n")
	for _, command := range commands {
		trimmedCommand := strings.TrimSpace(command)
		if strings.Contains(strings.ToLower(trimmedCommand), "no new") {
			i.logger.Info("No new libraries required.")
			return newInstalledPackagesMap, nil
		}
		command = strings.ReplaceAll(command, "-", "")
		var packageName string
		var version string
		if !beforeVersionUpdate {
			packageName, version = i.extractPackageNameAndVersion(command)
			// Check the current state of the package in installedPackages
			if currentVersion, exists := installedPackages[packageName]; exists {
				if currentVersion == version {
					// Same package and version, skip
					continue
				} else if version > currentVersion {
					// Upgrade needed
					newInstalledPackagesMap[packageName] = map[string]string{
						"version": version,
						"action":  "upgrade",
					}
				} else if version < currentVersion {
					// Downgrade needed
					newInstalledPackagesMap[packageName] = map[string]string{
						"version": version,
						"action":  "downgrade",
					}
				}
			} else {
				// New package to install
				newInstalledPackagesMap[packageName] = map[string]string{
					"version": version,
					"action":  "install",
				}
			}
		} else {
			packageName = i.extractPackageName(command)
			if _, exists := installedPackages[packageName]; exists {
				continue
			} else {
				// New package to install
				newInstalledPackagesMap[packageName] = map[string]string{
					"version": "dummy version",
					"action":  "install",
				}
			}
		}

		// Run the install command
		stdout, stderr, exitCode, _, err := RunCommand(command, "", i.logger)
		if exitCode != 0 || err != nil {
			i.logger.Info("FAILED TO INSTALL PACKAGE: " + packageName + " with command: " + command)
			i.logger.Info("ERROR: " + err.Error() + " , stdout: " + stdout + ", stderr: " + stderr)
			return nil, fmt.Errorf("failed to install library: %s, error: %w", command, err)
		}
	}
	return newInstalledPackagesMap, nil

}
func (i *Injector) installLibraries(libraryCommands string, installedPackages []string) ([]string, error) {
	var newInstalledPackages []string
	libraryCommands = strings.TrimSpace(libraryCommands)
	if libraryCommands == "" || libraryCommands == "\"\"" {
		return newInstalledPackages, nil
	}

	commands := strings.Split(libraryCommands, "\n")
	for _, command := range commands {
		command = strings.ReplaceAll(command, "-", "")
		packageName := i.extractPackageName(command)
		if isStringInarray(installedPackages, packageName) {
			continue
		}
		_, _, exitCode, _, err := RunCommand(command, "", i.logger)
		if exitCode != 0 || err != nil {
			return newInstalledPackages, fmt.Errorf("failed to install library: %s", command)
		}
		installedPackages = append(installedPackages, packageName)
		newInstalledPackages = append(newInstalledPackages, packageName)
	}
	return newInstalledPackages, nil
}

func (i *Injector) extractPackageName(command string) string {
	fields := strings.Fields(command)
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}
func (i *Injector) extractPackageNameAndVersion(command string) (string, string) {
	fields := strings.Fields(command)
	if i.language == "go" && len(fields) < 4 {
		return fields[2], ""
	}
	if len(fields) < 3 {
		return "", ""
	}
	return fields[2], fields[3]
}

func (i *Injector) uninstallLibraries2(installedPackagesMap map[string]string, newInstalledPackagesMap map[string]map[string]string) error {
	beforeVersionUpdate := true
	if !beforeVersionUpdate {
		for packageName, packageDetails := range newInstalledPackagesMap {
			newVersion := packageDetails["version"]

			if originalVersion, exists := installedPackagesMap[packageName]; exists {
				// If the package exists but the version is different
				if originalVersion != newVersion {
					i.logger.Info(fmt.Sprintf("Reverting package %s to original version %s (was %s).", packageName, originalVersion, newVersion))
					if err := i.installSpecificVersion(packageName, originalVersion); err != nil {
						i.logger.Warn(fmt.Sprintf("Failed to revert package %s to version %s.", packageName, originalVersion), zap.Error(err))
					}
				} else {
					// If the version matches, do nothing
					i.logger.Info(fmt.Sprintf("Package %s is already at the correct version (%s). Skipping.", packageName, originalVersion))
				}
			} else {
				// If the package doesn't exist in installedPackagesMap, uninstall it
				i.logger.Info(fmt.Sprintf("Uninstalling package %s (not part of the original state).", packageName))
				if err := i.uninstallSpecificPackage(packageName); err != nil {
					i.logger.Warn(fmt.Sprintf("Failed to uninstall package: %s.", packageName), zap.Error(err))
				}
			}
		}
	} else {
		for packageName := range newInstalledPackagesMap {

			if _, exists := installedPackagesMap[packageName]; exists {
				continue
			} else {
				// If the package doesn't exist in installedPackagesMap, uninstall it
				i.logger.Info(fmt.Sprintf("Uninstalling package %s (not part of the original state).", packageName))
				if err := i.uninstallSpecificPackage(packageName); err != nil {
					i.logger.Warn(fmt.Sprintf("Failed to uninstall package: %s.", packageName), zap.Error(err))
				}
			}
		}
	}

	return nil

}

func (i *Injector) installSpecificVersion(packageName, version string) error {
	var installCommand string
	switch strings.ToLower(i.language) {
	case "go":
		installCommand = fmt.Sprintf("go get %s@%s && go mod tidy", packageName, version)
	case "python":
		installCommand = fmt.Sprintf("pip install %s==%s", packageName, version)
	case "javascript":
		installCommand = fmt.Sprintf("npm install %s@%s", packageName, version)
	case "java":
		installCommand = fmt.Sprintf("mvn dependency:get -Dartifact=%s:%s", packageName, version)
	}
	if installCommand != "" {
		_, _, exitCode, _, err := RunCommand(installCommand, "", i.logger)
		if exitCode != 0 || err != nil {
			return fmt.Errorf("failed to install package %s@%s: %w", packageName, version, err)
		}
	}
	return nil
}

func (i *Injector) uninstallSpecificPackage(packageName string) error {
	var uninstallCommand string
	switch strings.ToLower(i.language) {
	case "go":
		uninstallCommand = fmt.Sprintf("go mod edit -droprequire %s && go mod tidy", packageName)
	case "python":
		uninstallCommand = fmt.Sprintf("pip uninstall -y %s", packageName)
	case "javascript":
		uninstallCommand = fmt.Sprintf("npm uninstall %s", packageName)
	case "java":
		uninstallCommand = fmt.Sprintf("mvn dependency:purge-local-repository -DreResolve=false -Dinclude=%s", packageName)
	}
	if uninstallCommand != "" {
		_, _, exitCode, _, err := RunCommand(uninstallCommand, "", i.logger)
		if exitCode != 0 || err != nil {
			return fmt.Errorf("failed to uninstall package %s: %w", packageName, err)
		}
	}
	return nil
}
func (i *Injector) uninstallLibraries(installedPackages []string) error {
	for _, command := range installedPackages {
		i.logger.Info(fmt.Sprintf("Uninstalling library: %s", command))

		var uninstallCommand string
		switch strings.ToLower(i.language) {
		case "go":
			uninstallCommand = fmt.Sprintf("go mod edit -droprequire %s && go mod tidy", command)
		case "python":
			uninstallCommand = fmt.Sprintf("pip uninstall -y %s", command)
		case "javascript":
			uninstallCommand = fmt.Sprintf("npm uninstall %s", command)
		case "java":
			uninstallCommand = fmt.Sprintf("mvn dependency:purge-local-repository -DreResolve=false -Dinclude=%s", command)
		}
		if uninstallCommand != "" {
			i.logger.Info(fmt.Sprintf("Uninstalling library with command: %s", uninstallCommand))
			_, _, exitCode, _, err := RunCommand(uninstallCommand, "", i.logger)
			if exitCode != 0 || err != nil {
				i.logger.Warn(fmt.Sprintf("Failed to uninstall library: %s", uninstallCommand), zap.Error(err))
			}
		}
	}
	return nil
}

func (i *Injector) updateJavaScriptImports(importedContent string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?m)^\s*(import\s+.*?from\s+['"].*?['"];?|const\s+.*?=\s+require\(['"].*?['"]\);?)`)
	existingImportsSet := make(map[string]bool)
	sanitisedImports := []string{}
	existingImports := importRegex.FindAllString(importedContent, -1)
	for _, imp := range existingImports {
		imp = strings.TrimSpace(imp)
		cleanedImport := strings.ReplaceAll(imp, " ", "")
		if cleanedImport != "" && !existingImportsSet[cleanedImport] {
			existingImportsSet[cleanedImport] = true
			sanitisedImports = append(sanitisedImports, imp)
		}
	}

	for _, imp := range newImports {
		imp = strings.Trim(imp, `"- `)
		cleanedImport := strings.ReplaceAll(imp, " ", "")
		if importRegex.MatchString(imp) && !existingImportsSet[cleanedImport] {
			existingImportsSet[cleanedImport] = true
			sanitisedImports = append(sanitisedImports, imp)
		}
	}
	updatedImports := strings.Join(sanitisedImports, "\n") + "\n\n"

	contentWithoutImports := importRegex.ReplaceAllString(importedContent, "")
	contentWithoutImports = strings.TrimLeft(contentWithoutImports, "\n")

	updatedContent := updatedImports + "\n" + contentWithoutImports

	originalLines := strings.Split(importedContent, "\n")
	updatedLines := strings.Split(updatedContent, "\n")
	importLength := len(updatedLines) - len(originalLines)

	if importLength < 0 {
		importLength = 0
	}

	return updatedContent, importLength, nil
}

func (i *Injector) updateImports(filePath string, imports string) (int, error) {
	importLines := strings.Split(imports, "\n")
	var newImports []string

	for _, imp := range importLines {
		trimmedImp := strings.TrimSpace(imp)
		if strings.Contains(trimmedImp, "No new imports") || strings.Contains(trimmedImp, "None") {
			continue
		}
		newImports = append(newImports, trimmedImp)
	}
	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	content := string(contentBytes)

	var updatedContent string
	var importLength int
	switch strings.ToLower(i.language) {
	case "go":
		updatedContent, importLength, err = i.updateGoImports(content, newImports)
	case "java":
		updatedContent, importLength, err = i.updateJavaImports(content, newImports)
	case "python":
		updatedContent, err = i.updatePythonImports(content, newImports)
	case "typescript":
		updatedContent, importLength, err = i.updateTypeScriptImports(content, newImports)
	case "javascript":
		updatedContent, importLength, err = i.updateJavaScriptImports(content, newImports)
	default:
		return 0, fmt.Errorf("unsupported language: %s", i.language)
	}
	if err != nil {
		return 0, err
	}
	err = os.WriteFile(filePath, []byte(updatedContent), fs.ModePerm)

	if err != nil {
		return 0, err
	}

	return importLength, nil
}

func (i *Injector) updateGoImports(codeBlock string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?ms)import\s*(\([\s\S]*?\)|"[^"]+")`)
	existingImportsSet := make(map[string]bool)
	matches := importRegex.FindStringSubmatch(codeBlock)
	if matches != nil {
		importBlock := matches[0]
		importLines := strings.Split(importBlock, "\n")
		allImports := []string{}
		existingImports := i.extractGoImports(importLines, true)
		for _, imp := range existingImports {
			trimmedImp := strings.TrimSpace(imp)
			if trimmedImp != "" {
				existingImportsSet[trimmedImp] = true
			}
			allImports = append(allImports, imp)
		}
		newImports = i.extractGoImports(newImports, false)
		for _, importStatement := range newImports {
			importStatement = strings.TrimSpace(importStatement)
			if !existingImportsSet[importStatement] {
				existingImportsSet[importStatement] = true
				allImports = append(allImports, importStatement)
			}
		}
		importBlockNew := i.createGoImportBlock(allImports)
		updatedContent := importRegex.ReplaceAllString(codeBlock, importBlockNew)
		return updatedContent, len(strings.Split(importBlockNew, "\n")) - len(importLines), nil
	}
	packageRegex := regexp.MustCompile(`package\s+\w+`)

	pkgMatch := packageRegex.FindStringIndex(codeBlock)
	if pkgMatch == nil {
		return "", 0, fmt.Errorf("could not find package declaration")
	}
	newImports = i.extractGoImports(newImports, false)
	importBlock := i.createGoImportBlock(newImports)
	insertPos := pkgMatch[1]
	updatedContent := codeBlock[:insertPos] + "\n\n" + importBlock + "\n" + codeBlock[insertPos:]
	return updatedContent, len(strings.Split(importBlock, "\n")) + 1, nil

}

func (i *Injector) extractGoImports(importLines []string, ignoreSpace bool) []string {
	imports := []string{}
	for _, line := range importLines {
		line = strings.TrimSpace(line)
		if line == "import (" || line == ")" {
			continue
		}
		if line == "" {
			if ignoreSpace {
				imports = append(imports, "")
			}
			continue
		}
		line = strings.TrimPrefix(line, "import ")
		line = strings.Trim(line, `"`)
		imports = append(imports, line)
	}
	return imports
}

func (i *Injector) createGoImportBlock(imports []string) string {
	importBlock := "import (\n"
	for _, importLine := range imports {
		importLine = strings.TrimSpace(importLine)
		if importLine == "" {
			importBlock += "\n"
			continue
		}
		importLine = strings.Trim(importLine, `"`)
		importBlock += fmt.Sprintf(`    "%s"`+"\n", importLine)
	}
	importBlock += ")"
	return importBlock
}

func (i *Injector) updateJavaImports(codeContent string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?m)^import\s+.*?;`)
	existingImportsSet := make(map[string]bool)
	existingImportMatches := importRegex.FindAllStringIndex(codeContent, -1)

	for _, match := range existingImportMatches {
		imp := codeContent[match[0]:match[1]]
		existingImportsSet[imp] = true
	}

	importsToAdd := []string{}
	for _, importStatement := range newImports {
		importStatement = strings.ReplaceAll(importStatement, "-", "")
		importStatement = strings.TrimSpace(importStatement)
		importStatement = strings.Trim(importStatement, "\"")
		if importRegex.MatchString(importStatement) && !existingImportsSet[importStatement] {
			existingImportsSet[importStatement] = true
			importsToAdd = append(importsToAdd, importStatement)
		}
	}
	if len(importsToAdd) > 0 {
		insertPos := 0
		if len(existingImportMatches) > 0 {
			lastImportMatch := existingImportMatches[len(existingImportMatches)-1]
			insertPos = lastImportMatch[1] // position after last existing import
		} else {
			packageRegex := regexp.MustCompile(`(?m)^package\s+.*?;`)
			pkgMatch := packageRegex.FindStringIndex(codeContent)
			if pkgMatch != nil {
				insertPos = pkgMatch[1]
			} else {
				insertPos = 0
			}
		}

		importedContent := "\n" + strings.Join(importsToAdd, "\n") + "\n"

		updatedContent := codeContent[:insertPos] + importedContent + codeContent[insertPos:]

		return updatedContent, len(importsToAdd), nil
	}
	return codeContent, 0, nil

}

func (i *Injector) updatePythonImports(content string, newImports []string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	existingImportsMap := make(map[string]map[string]bool)
	codeLines := []string{}
	importLines := []string{}

	ignoredPrefixes := "# checking coverage for file - do not remove"

	for scanner.Scan() {
		line := scanner.Text()
		trimmedLine := strings.TrimSpace(line)

		if trimmedLine == "" {
			continue
		}
		shouldIgnore := (strings.HasPrefix(trimmedLine, "import ") || strings.HasPrefix(trimmedLine, "from ")) && strings.Contains(trimmedLine, ignoredPrefixes)
		if shouldIgnore {
			parts := strings.Split(trimmedLine, "#")
			coreImport := strings.TrimSpace(parts[0])

			if strings.HasPrefix(coreImport, "from ") {
				fields := strings.Fields(coreImport)
				moduleName := fields[1]
				importPart := coreImport[strings.Index(coreImport, "import")+len("import "):]
				importedItems := strings.Split(importPart, ",")

				if _, exists := existingImportsMap[moduleName]; !exists {
					existingImportsMap[moduleName] = make(map[string]bool)
				}
				for _, item := range importedItems {
					cleanedItem := strings.TrimSpace(item)
					if cleanedItem != "" {
						existingImportsMap[moduleName][cleanedItem] = true
					}
				}
			}
			codeLines = append(codeLines, line)
			continue
		}

		if strings.HasPrefix(trimmedLine, "import ") || strings.HasPrefix(trimmedLine, "from ") {
			codeLines = append(codeLines, line)
		} else {
			codeLines = append(codeLines, line)
		}
	}

	for _, imp := range newImports {
		imp = strings.TrimSpace(imp)
		if imp == "\"\"" || len(imp) == 0 {
			continue
		}
		if strings.HasPrefix(imp, "from ") {
			fields := strings.Fields(imp)
			moduleName := fields[1]
			importIndex := -1
			for i, field := range fields {
				if field == "import" {
					importIndex = i
					break
				}
			}
			if importIndex == -1 {
				continue
			}
			importPart := strings.Join(fields[importIndex+1:], " ")
			importedItems := strings.Split(importPart, ",")
			if _, exists := existingImportsMap[moduleName]; !exists {
				existingImportsMap[moduleName] = make(map[string]bool)
			}
			for _, item := range importedItems {
				cleanedItem := strings.TrimSpace(item)
				if cleanedItem != "" {
					existingImportsMap[moduleName][strings.TrimSpace(item)] = true
				}
			}
		} else if strings.HasPrefix(imp, "import ") {
			fields := strings.Fields(imp)
			moduleName := fields[1]
			if _, exists := existingImportsMap[moduleName]; !exists {
				existingImportsMap[moduleName] = make(map[string]bool)
			}
		}
	}
	for i, line := range codeLines {
		trimmedLine := strings.TrimSpace(line)

		if strings.HasPrefix(trimmedLine, "from ") {
			fields := strings.Fields(trimmedLine)
			moduleName := fields[1]

			if itemsMap, exists := existingImportsMap[moduleName]; exists && len(itemsMap) > 0 {
				items := mapKeysToSortedSlice(itemsMap)
				importLine := fmt.Sprintf("from %s import %s", moduleName, strings.Join(items, ", "))

				if strings.Contains(trimmedLine, ignoredPrefixes) {
					importLine += " " + ignoredPrefixes
				}
				codeLines[i] = importLine
				delete(existingImportsMap, moduleName)
			}
		}
	}

	for module, itemsMap := range existingImportsMap {
		if len(itemsMap) > 0 {
			items := mapKeysToSortedSlice(itemsMap)
			importLine := fmt.Sprintf("from %s import %s", module, strings.Join(items, ", "))
			importLine += " " + ignoredPrefixes
			importLines = append(importLines, importLine)
		}
	}
	nonEmptyCodeLines := []string{}
	for _, line := range codeLines {
		if strings.TrimSpace(line) != "" {
			nonEmptyCodeLines = append(nonEmptyCodeLines, line)
		}
	}

	nonEmptyImportLines := []string{}
	for _, line := range importLines {
		if strings.TrimSpace(line) != "" {
			nonEmptyImportLines = append(nonEmptyImportLines, line)
		}
	}

	updatedContent := strings.Join(nonEmptyImportLines, "\n") + "\n" + strings.Join(nonEmptyCodeLines, "\n")
	return updatedContent, nil
}

func (i *Injector) updateTypeScriptImports(importedContent string, newImports []string) (string, int, error) {
	importRegex := regexp.MustCompile(`(?m)^import\s+.*?;`)
	existingImportsSet := make(map[string]bool)
	sanitisedImports := []string{}
	existingImports := importRegex.FindAllString(importedContent, -1)
	for _, imp := range existingImports {
		imp = strings.TrimSpace(imp)
		cleanedImport := strings.ReplaceAll(imp, " ", "")
		if cleanedImport != "" && !existingImportsSet[cleanedImport] {
			existingImportsSet[cleanedImport] = true
			sanitisedImports = append(sanitisedImports, imp)
		}
	}

	for _, imp := range newImports {
		imp = strings.Trim(imp, `"- `)
		cleanedImport := strings.ReplaceAll(imp, " ", "")
		if importRegex.MatchString(imp) && !existingImportsSet[cleanedImport] {
			existingImportsSet[cleanedImport] = true
			sanitisedImports = append(sanitisedImports, imp)
		}
	}
	updatedImports := strings.Join(sanitisedImports, "\n") + "\n\n"

	contentWithoutImports := importRegex.ReplaceAllString(importedContent, "")
	contentWithoutImports = strings.TrimLeft(contentWithoutImports, "\n")

	updatedContent := updatedImports + "\n" + contentWithoutImports

	originalLines := strings.Split(importedContent, "\n")
	updatedLines := strings.Split(updatedContent, "\n")
	importLength := len(updatedLines) - len(originalLines)

	if importLength < 0 {
		importLength = 0
	}
	return updatedContent, importLength, nil
}

func (i *Injector) addCommentToTest(testCode string) string {
	comment := " Test generated using Keploy"
	switch i.language {
	case "python":
		comment = "#" + comment
	case "go", "javascript", "typescript", "java":
		comment = "//" + comment
	}
	return fmt.Sprintf("%s\n%s", comment, testCode)
}

func (i *Injector) getModelDetails(sourceFilePath string) string {
	switch i.language {
	case "go":
		return i.getGoImportData(sourceFilePath)
	default:
		return ""
	}
}

func (i *Injector) getGoImportData(sourceFilePath string) string {
	builtInTypes := map[string]struct{}{
		"string":     {},
		"int":        {},
		"float64":    {},
		"bool":       {},
		"error":      {},
		"byte":       {},
		"rune":       {},
		"uint":       {},
		"int64":      {},
		"uint64":     {},
		"complex64":  {},
		"complex128": {},
		"float32":    {},
		"int32":      {},
	}

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, sourceFilePath, nil, parser.AllErrors)
	if err != nil {
		return ""
	}

	imports := make(map[string]string)
	for _, imp := range node.Imports {
		pkgPath := strings.Trim(imp.Path.Value, "\"")
		var alias string

		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			alias = imp.Name.Name
		} else {
			parts := strings.Split(pkgPath, "/")
			alias = parts[len(parts)-1]
		}

		imports[alias] = pkgPath
	}
	// Set to store unique structs with their package paths
	structSet := make(map[string]struct{})
	funcSet := make(map[string]struct{})

	var collectStructs func(expr ast.Expr)
	collectStructs = func(expr ast.Expr) {
		switch t := expr.(type) {
		case *ast.Ident:
			structName := t.Name
			if _, isBuiltIn := builtInTypes[structName]; isBuiltIn {
				return
			}
			structKey := fmt.Sprintf("%s.%s", node.Name.Name, structName)
			structSet[structKey] = struct{}{}

		case *ast.SelectorExpr:
			if ident, ok := t.X.(*ast.Ident); ok {
				pkgAlias := ident.Name
				structName := t.Sel.Name
				if pkgPath, exists := imports[pkgAlias]; exists {
					structKey := fmt.Sprintf("%s.%s", pkgPath, structName)
					structSet[structKey] = struct{}{}
				} else {
					structKey := fmt.Sprintf("%s.%s", pkgAlias, structName)
					structSet[structKey] = struct{}{}
				}
			}

		case *ast.StarExpr:
			collectStructs(t.X)

		case *ast.ArrayType:
			collectStructs(t.Elt)

		case *ast.MapType:
			collectStructs(t.Key)
			collectStructs(t.Value)

		case *ast.StructType:
			packageName := node.Name.Name
			structSet[fmt.Sprintf("%s.<anonymous_struct>", packageName)] = struct{}{}
		}
	}

	// Traverse the AST to collect structs
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeSpec:
			if _, ok := x.Type.(*ast.StructType); ok {
				structName := x.Name.Name
				packageName := node.Name.Name
				structKey := fmt.Sprintf("%s.%s", packageName, structName)
				structSet[structKey] = struct{}{}
			}

		case *ast.CompositeLit:
			collectStructs(x.Type)

		case *ast.ValueSpec:
			if x.Type != nil {
				collectStructs(x.Type)
			}
			for _, val := range x.Values {
				collectStructs(val)
			}

		case *ast.Field:
			collectStructs(x.Type)

		case *ast.FuncDecl:
			if x.Type.Params != nil {
				for _, field := range x.Type.Params.List {
					collectStructs(field.Type)
				}
			}
			if x.Type.Results != nil {
				for _, field := range x.Type.Results.List {
					collectStructs(field.Type)
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					pkgAlias := ident.Name
					funcName := sel.Sel.Name
					if pkgPath, exists := imports[pkgAlias]; exists {
						// Construct the fully qualified function name
						funcKey := fmt.Sprintf("%s.%s", pkgPath, funcName)
						funcSet[funcKey] = struct{}{}
					}
				}
			} else if ident, ok := x.Fun.(*ast.Ident); ok {
				moduleName, relativePath := i.GetModuleName(sourceFilePath)
				packageName, _ := GetPackageName(sourceFilePath)

				if packageName != "main" {
					relativePath = TrimParentFolder(relativePath)
				}
				var funcKey string
				// Construct the function key conditionally to handle empty relativePath
				if packageName == "main" {
					// If the package is `main`, use the module name without extra path details
					funcKey = fmt.Sprintf("%s/%s.%s", moduleName, relativePath, ident.Name)
				} else {
					if relativePath == "" {
						funcKey = fmt.Sprintf("%s/%s.%s", moduleName, packageName, ident.Name)
					} else {
						funcKey = fmt.Sprintf("%s/%s/%s.%s", moduleName, relativePath, packageName, ident.Name)
					}
				}
				funcSet[funcKey] = struct{}{}
			}

		default:
		}
		return true
	})

	data := ""
	for structKey := range structSet {
		var out bytes.Buffer
		cmd := exec.Command("go", "doc", structKey)
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			continue
		}
		data += (out.String()) + "\n"
	}
	for funcKey := range funcSet {
		var out bytes.Buffer
		cmd := exec.Command("go", "doc", funcKey)
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			continue
		}
		data += (out.String()) + "\n"
	}
	return data
}

func (i *Injector) GetModuleName(sourceFilePath string) (string, string) {
	file, err := os.Open("go.mod")
	if err != nil {
		return "", ""
	}
	defer func() {
		if err := file.Close(); err != nil {
			i.logger.Error("Error closing file", zap.Error(err))
		}
	}()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "module ") {
			curDir, _ := os.Getwd()
			dirPath := filepath.Dir(sourceFilePath)

			relativePath, _ := filepath.Rel(curDir, dirPath)

			if relativePath == "." {
				return strings.TrimSpace(strings.TrimPrefix(line, "module ")), ""
			}

			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), relativePath
		}
	}

	return "", ""
}

func GetPackageName(sourceFilePath string) (string, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, sourceFilePath, nil, parser.PackageClauseOnly)
	if err != nil {
		return "", err
	}
	return node.Name.Name, nil
}

func TrimParentFolder(path string) string {
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) > 1 {
		return filepath.Join(parts[:len(parts)-1]...) // Exclude the last part (file name's parent directory)
	}
	return path
}
