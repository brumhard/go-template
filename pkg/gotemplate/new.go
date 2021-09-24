package gotemplate

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"github.com/schwarzit/go-template/config"
	"github.com/schwarzit/go-template/pkg/option"
	"sigs.k8s.io/yaml"
)

var ErrAlreadyExists = errors.New("already exists")

// TODO: add validation for opts
type NewRepositoryOptions struct {
	CWD          string
	ConfigValues map[string]interface{}
}

// LoadConfigValuesFromFile loads value for the options from a file.
func (gt *GT) LoadConfigValuesFromFile(file string) (map[string]interface{}, error) {
	fileBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	configValues := make(map[string]interface{}, len(gt.Configs.Integrations)+len(gt.Configs.Parameters))
	if err := yaml.Unmarshal(fileBytes, &configValues); err != nil {
		return nil, err
	}

	// TODO: make sure that all options are set if needed?

	return configValues, nil
}

func (gt *GT) LoadConfigValuesInteractively() (map[string]interface{}, error) {
	// TODO: add validation for value (probably regex pattern)

	gt.printBanner()
	configValues := make(map[string]interface{}, len(gt.Configs.Integrations)+len(gt.Configs.Parameters))
	for _, currentOption := range gt.Configs.Parameters {
		// Fix implicit memory aliasing (gosec G601)
		currentOption := currentOption
		if !dependenciesMet(&currentOption, configValues) {
			continue
		}

		// default value could contain templating functions
		var err error
		currentOption.Default, err = gt.applyTemplate(currentOption.Default, configValues)
		if err != nil {
			return nil, err
		}

		val, err := gt.readOptionValue(&currentOption)
		if err != nil {
			return nil, err
		}

		configValues[currentOption.Name] = val
	}

	for _, currentOption := range gt.Configs.Parameters {
		// Fix implicit memory aliasing (gosec G601)
		currentOption := currentOption
		if !dependenciesMet(&currentOption, configValues) {
			continue
		}

		// default value could contain templating functions
		var err error
		currentOption.Default, err = gt.applyTemplate(currentOption.Default, configValues)
		if err != nil {
			return nil, err
		}

		val, err := gt.readOptionValue(&currentOption)
		if err != nil {
			return nil, err
		}

		configValues[currentOption.Name] = val
	}

	return configValues, nil
}

func dependenciesMet(opt *option.Option, configValues map[string]interface{}) bool {
	if len(opt.DependsOn) == 0 {
		return true
	}

	for _, dep := range opt.DependsOn {
		depVal, ok := configValues[dep]
		if !ok {
			// if not found it means it not set
			return false
		}

		depBoolVal, ok := depVal.(bool)
		if !ok {
			// value will only be checked for bool values
			continue
		}

		if !depBoolVal {
			return false
		}
	}

	return true
}

func (gt *GT) InitNewProject(opts *NewRepositoryOptions) (err error) {
	gt.printProgress("Generating repo folder...")

	targetDir := path.Join(opts.CWD, opts.ConfigValues["projectSlug"].(string))
	gt.printProgress(fmt.Sprintf("Writing to %s...", targetDir))

	if _, err := os.Stat(targetDir); !os.IsNotExist(err) {
		return errors.Wrapf(ErrAlreadyExists, "directory %s", targetDir)
	}

	defer func() {
		if err != nil {
			// ignore error to not overwrite original error
			_ = os.RemoveAll(targetDir)
		}
	}()
	err = fs.WalkDir(config.TemplateDir, config.TemplateKey, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		pathToWrite, err := gt.executeTemplateString(path, opts.ConfigValues)
		if err != nil {
			return err
		}

		pathToWrite = strings.ReplaceAll(pathToWrite, config.TemplateKey, targetDir)
		if d.IsDir() {
			return os.MkdirAll(pathToWrite, os.ModePerm)
		}

		fileBytes, err := fs.ReadFile(config.TemplateDir, path)
		if err != nil {
			return err
		}

		data, err := gt.executeTemplateString(string(fileBytes), opts.ConfigValues)
		if err != nil {
			return err
		}

		return os.WriteFile(pathToWrite, []byte(data), os.ModePerm)
	})
	if err != nil {
		return err
	}

	gt.printProgress("Removing obsolete files of unused integrations...")
	if err := postHook(targetDir, gt.Configs.Integrations, opts.ConfigValues); err != nil {
		return err
	}

	gt.printProgress("Initializing git and Go modules...")
	if err := initRepo(targetDir, opts.ConfigValues["moduleName"].(string)); err != nil {
		return err
	}

	return nil
}

func initRepo(targetDir, moduleName string) error {
	gitInit := exec.Command("git", "init")
	gitInit.Dir = targetDir

	if err := gitInit.Run(); err != nil {
		return err
	}

	goModInit := exec.Command("go", "mod", "init", moduleName)
	goModInit.Dir = targetDir

	return goModInit.Run()
}

func postHook(targetDir string, options []option.Option, configValues map[string]interface{}) error {
	var toDelete []string

	for _, opt := range options {
		optEnabled, ok := configValues[opt.Name].(bool)
		if !ok {
			// if not bool value, files will be ignored
			continue
		}

		if optEnabled {
			toDelete = append(toDelete, opt.Files.Remove...)
			continue
		}
		// the files are added in the loop anyways, but if the option is disabled they should be removed again
		toDelete = append(toDelete, opt.Files.Add...)
	}

	for _, item := range toDelete {
		if err := os.RemoveAll(path.Join(targetDir, item)); err != nil {
			return err
		}
	}

	return nil
}

// readOptionValue reads a value for an option from the cli.
func (gt *GT) readOptionValue(opts *option.Option) (interface{}, error) {
	gt.printOption(opts)
	defer fmt.Fprintln(gt.Out)

	s, err := gt.readStdin()
	if err != nil {
		return nil, err
	}

	if s == "" {
		return opts.Default, nil
	}

	switch opts.Default.(type) {
	case string:
		return s, nil
	case bool:
		return strconv.ParseBool(s)
	case int:
		return strconv.Atoi(s)
	default:
		panic("unsupported type")
	}
}

func (gt *GT) readStdin() (string, error) {
	if ok := gt.InScanner.Scan(); !ok {
		return "", gt.InScanner.Err()
	}

	return strings.TrimSpace(gt.InScanner.Text()), nil
}

// applyTemplate executes a the template in the defaultValue with the valueMap as data.
// If the defaultValue is not a string, the input defaultValue will be returned.
func (gt *GT) applyTemplate(defaultValue interface{}, valueMap map[string]interface{}) (interface{}, error) {
	defaultStr, ok := defaultValue.(string)
	if !ok {
		return defaultValue, nil
	}

	return gt.executeTemplateString(defaultStr, valueMap)
}

// executeTemplateString executes the template in input str with the default p.FuncMap and valueMap as data.
func (gt *GT) executeTemplateString(str string, valueMap map[string]interface{}) (string, error) {
	tmpl, err := template.New("").Funcs(gt.FuncMap).Parse(str)
	if err != nil {
		return "", err
	}

	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, valueMap); err != nil {
		return "", err
	}

	return buffer.String(), nil
}
