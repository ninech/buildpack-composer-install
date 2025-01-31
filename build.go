package composer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paketo-buildpacks/packit/v2"
	"github.com/paketo-buildpacks/packit/v2/chronos"
	"github.com/paketo-buildpacks/packit/v2/draft"
	"github.com/paketo-buildpacks/packit/v2/fs"
	"github.com/paketo-buildpacks/packit/v2/pexec"
	"github.com/paketo-buildpacks/packit/v2/sbom"
	"github.com/paketo-buildpacks/packit/v2/scribe"
)

const (
	runComposerInstallOnCacheEnv = "BP_RUN_COMPOSER_INSTALL"
	opensslExtension             = "openssl"
)

// DetermineComposerInstallOptions defines the interface to get options for `composer install`
//
//go:generate faux --interface DetermineComposerInstallOptions --output fakes/determine_composer_install_options.go
type DetermineComposerInstallOptions interface {
	Determine() []string
}

// Executable just provides a fake for pexec.Executable for testing
//
//go:generate faux --interface Executable --output fakes/executable.go
type Executable interface {
	Execute(pexec.Execution) (err error)
}

//go:generate faux --interface SBOMGenerator --output fakes/sbom_generator.go
type SBOMGenerator interface {
	Generate(dir string) (sbom.SBOM, error)
}

// Calculator defines the interface for calculating a checksum of the given set
// of file paths.
//
//go:generate faux --interface Calculator --output fakes/calculator.go
type Calculator interface {
	Sum(paths ...string) (string, error)
}

func Build(
	logger scribe.Emitter,
	composerInstallOptions DetermineComposerInstallOptions,
	composerConfigExec Executable,
	composerInstallExec Executable,
	composerGlobalExec Executable,
	checkPlatformReqsExec Executable,
	sbomGenerator SBOMGenerator,
	path string,
	calculator Calculator,
	clock chronos.Clock) packit.BuildFunc {
	return func(context packit.BuildContext) (packit.BuildResult, error) {
		logger.Title("%s %s", context.BuildpackInfo.Name, context.BuildpackInfo.Version)

		composerPhpIniPath, err := writeComposerPhpIni(logger, context)
		if err != nil { // untested
			return packit.BuildResult{}, err
		}

		composerGlobalBin, err := runComposerGlobalIfRequired(logger, context, composerGlobalExec, path, composerPhpIniPath)
		if err != nil { // untested
			return packit.BuildResult{}, err
		}

		if composerGlobalBin != "" {
			path = strings.Join([]string{
				composerGlobalBin,
				path,
			}, string(os.PathListSeparator))
		}

		workspaceVendorDir := filepath.Join(context.WorkingDir, "vendor")

		if value, found := os.LookupEnv(ComposerVendorDir); found {
			workspaceVendorDir = filepath.Join(context.WorkingDir, value)
		}

		var composerPackagesLayer packit.Layer
		logger.Process("Executing build process")
		duration, err := clock.Measure(func() error {
			composerPackagesLayer, err = runComposerInstall(
				logger,
				context,
				composerInstallOptions,
				composerPhpIniPath,
				path,
				composerConfigExec,
				composerInstallExec,
				workspaceVendorDir,
				calculator)
			return err
		})
		if err != nil {
			return packit.BuildResult{}, err
		}
		logger.Action("Completed in %s", duration.Round(time.Millisecond))
		logger.Break()

		logger.GeneratingSBOM(composerPackagesLayer.Path)

		var sbomContent sbom.SBOM
		duration, err = clock.Measure(func() error {
			sbomContent, err = sbomGenerator.Generate(context.WorkingDir)
			return err
		})
		if err != nil {
			return packit.BuildResult{}, err
		}
		logger.Action("Completed in %s", duration.Round(time.Millisecond))
		logger.Break()

		logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)

		composerPackagesLayer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
		if err != nil {
			return packit.BuildResult{}, err
		}

		err = runCheckPlatformReqs(logger, checkPlatformReqsExec, context.WorkingDir, composerPhpIniPath, path)
		if err != nil {
			return packit.BuildResult{}, err
		}

		return packit.BuildResult{
			Layers: []packit.Layer{
				composerPackagesLayer,
			},
		}, nil
	}
}

// runComposerGlobalIfRequired will check for existence of env var "BP_COMPOSER_INSTALL_GLOBAL".
// If that exists, will run `composer global require` with the contents of BP_COMPOSER_INSTALL_GLOBAL
// to ensure that those packages are available for Composer scripts.
//
// It will return the location to which the packages have been installed, so that they can be made available
// on the path when running `composer install`.
//
// `composer global require`: https://getcomposer.org/doc/03-cli.md#global
// Composer scripts: https://getcomposer.org/doc/articles/scripts.md
func runComposerGlobalIfRequired(
	logger scribe.Emitter,
	context packit.BuildContext,
	composerGlobalExec Executable,
	path string,
	composerPhpIniPath string) (composerGlobalBin string, err error) {
	composerInstallGlobal, found := os.LookupEnv(BpComposerInstallGlobal)

	if !found {
		return "", nil
	}

	composerGlobalLayer, err := context.Layers.Get(ComposerGlobalLayerName)
	if err != nil { // untested
		return "", err
	}

	composerGlobalLayer, err = composerGlobalLayer.Reset()
	if err != nil { // untested
		return "", err
	}

	globalPackages := strings.Split(composerInstallGlobal, " ")
	args := append([]string{"global", "require", "--no-progress"}, globalPackages...)
	logger.Process("Running 'composer %s'", strings.Join(args, " "))

	execution := pexec.Execution{
		Args: args,
		Dir:  composerGlobalLayer.Path,
		Env: append(os.Environ(),
			"COMPOSER_NO_INTERACTION=1", // https://getcomposer.org/doc/03-cli.md#composer-no-interaction
			fmt.Sprintf("COMPOSER_HOME=%s", composerGlobalLayer.Path),
			fmt.Sprintf("PHPRC=%s", composerPhpIniPath),
			"COMPOSER_VENDOR_DIR=vendor", // ensure default in the layer
			fmt.Sprintf("PATH=%s", path),
		),
		Stdout: logger.ActionWriter,
		Stderr: logger.ActionWriter,
	}
	err = composerGlobalExec.Execute(execution)
	if err != nil {
		return "", err
	}

	composerGlobalBin = filepath.Join(composerGlobalLayer.Path, "vendor", "bin")

	if os.Getenv(BpLogLevel) == "DEBUG" {
		logger.Debug.Subprocess("Adding global Composer packages to PATH:")
		files, err := os.ReadDir(composerGlobalBin)
		if err != nil { // untested
			return "", err
		}
		for _, f := range files {
			logger.Debug.Subprocess(fmt.Sprintf("- %s", f.Name()))
		}
	}

	return
}

// runComposerInstall will run `composer install` to download dependencie into
// the app directory, and will be copied into a layer and cached for reuse.
//
// Returns:
// - composerPackagesLayer: a new layer into which the dependencies will be installed
// - err: any error
func runComposerInstall(
	logger scribe.Emitter,
	context packit.BuildContext,
	composerInstallOptions DetermineComposerInstallOptions,
	composerPhpIniPath string,
	path string,
	composerConfigExec Executable,
	composerInstallExec Executable,
	workspaceVendorDir string,
	calculator Calculator) (composerPackagesLayer packit.Layer, err error) {

	launch, build := draft.NewPlanner().MergeLayerTypes(ComposerPackagesDependency, context.Plan.Entries)

	composerPackagesLayer, err = context.Layers.Get(ComposerPackagesLayerName)
	if err != nil { // untested
		return packit.Layer{}, err
	}

	composerJsonPath, composerLockPath, _, _ := FindComposerFiles(context.WorkingDir)

	layerVendorDir := filepath.Join(composerPackagesLayer.Path, "vendor")

	composerLockChecksum, err := calculator.Sum(composerLockPath)
	if err != nil { // untested
		return packit.Layer{}, err
	}

	logger.Debug.Process("Calculated checksum of %s for composer.lock", composerLockChecksum)

	stack, stackOk := composerPackagesLayer.Metadata["stack"]
	if stackOk {
		logger.Debug.Process("Previous stack: %s", stack.(string))
		logger.Debug.Process("Current stack: %s", context.Stack)
	}

	cachedSHA, shaOk := composerPackagesLayer.Metadata["composer-lock-sha"].(string)
	if (shaOk && cachedSHA == composerLockChecksum) && (stackOk && stack.(string) == context.Stack) {
		logger.Process("Reusing cached layer %s", composerPackagesLayer.Path)
		logger.Break()

		composerPackagesLayer.Launch, composerPackagesLayer.Build = launch, build
		// the layer is always set to cache = true because we need it during subsequent builds to copy vendor into /workspace
		composerPackagesLayer.Cache = true

		logger.Debug.Subprocess("Setting cached layer types: launch=[%t], build=[%t], cache=[%t]",
			composerPackagesLayer.Launch,
			composerPackagesLayer.Build,
			composerPackagesLayer.Cache)

		if os.Getenv(BpLogLevel) == "DEBUG" {
			logger.Debug.Subprocess("Listing files in %s:", composerPackagesLayer)
			files, err := os.ReadDir(composerPackagesLayer.Path)
			if err != nil { // untested
				return packit.Layer{}, err
			}
			for _, f := range files {
				logger.Debug.Subprocess(fmt.Sprintf("- %s", f.Name()))
			}
		}
		// we run "composer install" again on the cached content as
		// sometimes composer modules install certain things to special
		// directories other than the "vendor" directory.  See:
		// https://getcomposer.org/doc/faqs/how-do-i-install-a-package-to-a-custom-path-for-my-framework.md
		// for more information. This can be switched off by setting
		// the environment variable "BP_RUN_COMPOSER_INSTALL" to false.
		runComposerInstallOnCache := true
		runComposerInstallStr, found := os.LookupEnv(runComposerInstallOnCacheEnv)
		if found {
			var err error
			if runComposerInstallOnCache, err = strconv.ParseBool(runComposerInstallStr); err != nil {
				return packit.Layer{}, fmt.Errorf("error when parsing env var %q: %w", runComposerInstallOnCacheEnv, err)
			}
		}

		if runComposerInstallOnCache {
			installArgs := append([]string{"install"}, composerInstallOptions.Determine()...)
			logger.Process("Running 'composer %s' from cached files", strings.Join(installArgs, " "))

			// install packages into /workspace/vendor because composer cannot handle symlinks easily
			execution := pexec.Execution{
				Args: installArgs,
				Dir:  context.WorkingDir,
				Env: append(os.Environ(),
					"COMPOSER_NO_INTERACTION=1", // https://getcomposer.org/doc/03-cli.md#composer-no-interaction
					fmt.Sprintf("COMPOSER=%s", composerJsonPath),
					fmt.Sprintf("COMPOSER_HOME=%s", filepath.Join(composerPackagesLayer.Path, ".composer")),
					fmt.Sprintf("COMPOSER_VENDOR_DIR=%s", workspaceVendorDir),
					fmt.Sprintf("PHPRC=%s", composerPhpIniPath),
					fmt.Sprintf("PATH=%s", path),
				),
				Stdout: logger.ActionWriter,
				Stderr: logger.ActionWriter,
			}
			err = composerInstallExec.Execute(execution)
			if err != nil {
				return packit.Layer{}, err
			}
		}

		if exists, err := fs.Exists(workspaceVendorDir); err != nil {
			return packit.Layer{}, err
		} else if exists {
			logger.Process("Detected existing vendored packages, replacing with cached vendored packages")
			if err := os.RemoveAll(workspaceVendorDir); err != nil { // untested
				return packit.Layer{}, err
			}
		}

		if err := fs.Copy(layerVendorDir, workspaceVendorDir); err != nil { // untested
			return packit.Layer{}, err
		}

		return composerPackagesLayer, nil
	}

	logger.Process("Building new layer %s", composerPackagesLayer.Path)

	composerPackagesLayer, err = composerPackagesLayer.Reset()
	if err != nil { // untested
		return packit.Layer{}, err
	}

	composerPackagesLayer.Launch, composerPackagesLayer.Build = launch, build
	// the layer is always set to cache = true because we need it during subsequent builds to copy vendor into /workspace
	composerPackagesLayer.Cache = true

	logger.Debug.Subprocess("Setting layer types: launch=[%t], build=[%t], cache=[%t]",
		composerPackagesLayer.Launch,
		composerPackagesLayer.Build,
		composerPackagesLayer.Cache)

	composerPackagesLayer.Metadata = map[string]interface{}{
		"stack":             context.Stack,
		"composer-lock-sha": composerLockChecksum,
	}

	args := []string{"config", "autoloader-suffix", ComposerAutoloaderSuffix}
	logger.Process("Running 'composer %s'", strings.Join(args, " "))

	execution := pexec.Execution{
		Args: args,
		Dir:  composerPackagesLayer.Path,
		Env: append(os.Environ(),
			"COMPOSER_NO_INTERACTION=1", // https://getcomposer.org/doc/03-cli.md#composer-no-interaction
			fmt.Sprintf("COMPOSER=%s", composerJsonPath),
			fmt.Sprintf("COMPOSER_HOME=%s", filepath.Join(composerPackagesLayer.Path, ".composer")),
			"COMPOSER_VENDOR_DIR=vendor", // ensure default in the layer
			fmt.Sprintf("PHPRC=%s", composerPhpIniPath),
			fmt.Sprintf("PATH=%s", path),
		),
		Stdout: logger.ActionWriter,
		Stderr: logger.ActionWriter,
	}

	err = composerConfigExec.Execute(execution)
	if err != nil {
		return packit.Layer{}, err
	}

	// `composer install` will run with `--no-autoloader` to avoid errors from
	// autoloading classes outside of the vendor directory

	// Once `composer install` has run, the symlink to the working directory is
	// set up, and then `composer dump-autoload` on the vendor directory from
	// the working directory.

	installArgs := append([]string{"install"}, composerInstallOptions.Determine()...)
	logger.Process("Running 'composer %s'", strings.Join(installArgs, " "))

	// install packages into /workspace/vendor because composer cannot handle symlinks easily
	execution = pexec.Execution{
		Args: installArgs,
		Dir:  context.WorkingDir,
		Env: append(os.Environ(),
			"COMPOSER_NO_INTERACTION=1", // https://getcomposer.org/doc/03-cli.md#composer-no-interaction
			fmt.Sprintf("COMPOSER=%s", composerJsonPath),
			fmt.Sprintf("COMPOSER_HOME=%s", filepath.Join(composerPackagesLayer.Path, ".composer")),
			fmt.Sprintf("COMPOSER_VENDOR_DIR=%s", workspaceVendorDir),
			fmt.Sprintf("PHPRC=%s", composerPhpIniPath),
			fmt.Sprintf("PATH=%s", path),
		),
		Stdout: logger.ActionWriter,
		Stderr: logger.ActionWriter,
	}
	err = composerInstallExec.Execute(execution)
	if err != nil {
		return packit.Layer{}, err
	}

	logger.Process("Copying from %s => to %s", workspaceVendorDir, layerVendorDir)

	err = fs.Copy(workspaceVendorDir, layerVendorDir)
	if err != nil {
		return packit.Layer{}, err
	}

	if os.Getenv(BpLogLevel) == "DEBUG" {
		logger.Debug.Subprocess("Listing files in %s:", layerVendorDir)
		files, err := os.ReadDir(layerVendorDir)
		if err != nil { // untested
			return packit.Layer{}, err
		}
		for _, f := range files {
			logger.Debug.Subprocess(fmt.Sprintf("- %s", f.Name()))
		}
	}

	return composerPackagesLayer, nil
}

// writeComposerPhpIni will create a PHP INI file used by Composer itself,
// such as when running `composer global` and `composer install.
// This is created in a new ignored layer.
func writeComposerPhpIni(logger scribe.Emitter, context packit.BuildContext) (composerPhpIniPath string, err error) {
	composerPhpIniLayer, err := context.Layers.Get(ComposerPhpIniLayerName)
	if err != nil { // untested
		return "", err
	}

	composerPhpIniLayer, err = composerPhpIniLayer.Reset()
	if err != nil { // untested
		return "", err
	}

	composerPhpIniPath = filepath.Join(composerPhpIniLayer.Path, "composer-php.ini")

	logger.Debug.Process("Writing php.ini for composer")
	logger.Debug.Subprocess("Writing %s to %s", filepath.Base(composerPhpIniPath), composerPhpIniPath)

	phpIni := fmt.Sprintf(`[PHP]
extension_dir = "%s"
extension = %s.so`, os.Getenv(PhpExtensionDir), opensslExtension)
	logger.Debug.Subprocess("Writing php.ini contents:\n'%s'", phpIni)

	return composerPhpIniPath, os.WriteFile(composerPhpIniPath, []byte(phpIni), os.ModePerm)
}

// runCheckPlatformReqs will run Composer command `check-platform-reqs`
// to see which platform requirements are "missing".
// https://getcomposer.org/doc/03-cli.md#check-platform-reqs
//
// Any "missing" requirements will be added to an INI file that should be autoloaded via PHP_INI_SCAN_DIR,
// when used in conjunction with the `php-dist` Paketo Buildpack
// INI file location: {workingDir}/.php.ini.d/composer-extensions.ini
// PHP_INI_SCAN_DIR: https://github.com/paketo-buildpacks/php-dist/blob/bfed65e9c3b59cf2c5aee3752d82470f8259f655/build.go#L219-L223
// Requires `php-dist` 0.8.0+ (https://github.com/paketo-buildpacks/php-dist/releases/tag/v0.8.0)
//
// This code has been largely borrowed from the original `php-composer` buildpack
// https://github.com/paketo-buildpacks/php-composer/blob/5e2604b74cbeb30090bf7eadb1cfc158b374efc0/composer/composer.go#L76-L100
//
// In case you are curious about exit code 2: https://getcomposer.org/doc/03-cli.md#process-exit-codes
func runCheckPlatformReqs(logger scribe.Emitter, checkPlatformReqsExec Executable, workingDir, composerPhpIniPath, path string) error {

	args := []string{"check-platform-reqs"}
	logger.Process("Running 'composer %s'", strings.Join(args, " "))
	buffer := bytes.NewBuffer(nil)
	execution := pexec.Execution{
		Args: args,
		Dir:  workingDir,
		Env: append(os.Environ(),
			"COMPOSER_NO_INTERACTION=1", // https://getcomposer.org/doc/03-cli.md#composer-no-interaction
			fmt.Sprintf("PHPRC=%s", composerPhpIniPath),
			fmt.Sprintf("PATH=%s", path),
		),
		Stdout: io.MultiWriter(logger.ActionWriter, buffer),
		Stderr: io.MultiWriter(logger.ActionWriter, buffer),
	}

	err := checkPlatformReqsExec.Execute(execution)
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok || exitError.ExitCode() != 2 {
			return err
		}
	}

	// we always include the openssl extension as it will not be found
	// otherwise. The reason for this is that `writeComposerPhpIni` gets
	// executed first and already includes the openssl extension. `composer
	// check-platform-reqs` will therefore not output a missing openssl
	// extension (as it was already loaded).
	var extensions = []string{opensslExtension}
	for _, line := range strings.Split(buffer.String(), "\n") {
		chunks := strings.Split(strings.TrimSpace(line), " ")
		extensionName := strings.TrimPrefix(strings.TrimSpace(chunks[0]), "ext-")
		extensionStatus := strings.TrimSpace(chunks[len(chunks)-1])
		if extensionName != "php" && extensionName != "php-64bit" && extensionStatus == "missing" {
			extensions = append(extensions, extensionName)
		}
	}

	logger.Process("Found extensions '%s'", strings.Join(extensions, ", "))

	buf := bytes.Buffer{}

	for _, extension := range extensions {
		buf.WriteString(fmt.Sprintf("extension = %s.so\n", extension))
	}

	iniDir := filepath.Join(workingDir, ".php.ini.d")

	err = os.MkdirAll(iniDir, os.ModeDir|os.ModePerm)
	if err != nil { // untested
		return err
	}

	return os.WriteFile(filepath.Join(iniDir, "composer-extensions.ini"), buf.Bytes(), 0666)
}
