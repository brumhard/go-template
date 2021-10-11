package gotemplate

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	gotemplate "github.com/schwarzit/go-template"
	"sigs.k8s.io/yaml"
)

var (
	ErrAlreadyExists   = errors.New("already exists")
	ErrParameterNotSet = errors.New("parameter not set")
	ErrMalformedInput  = errors.New("malformed input")
	ErrParameterSet    = errors.New("parameter set but preconditions are not met")
)

type ErrTypeMismatch struct {
	Expected string
	Actual   string
}

func (e *ErrTypeMismatch) Error() string {
	return fmt.Sprintf("type mismatch, got %s, expected %s", e.Actual, e.Expected)
}

type NewRepositoryOptions struct {
	OutputDir    string
	OptionValues *OptionValues
}

// Validate validates all properties of NewRepositoryOptions except the ConfigValues, since those are validated by the Load functions.
func (opts NewRepositoryOptions) Validate() error {
	if opts.OutputDir == "" {
		return nil
	}

	if _, err := os.Stat(opts.OutputDir); err != nil {
		return err
	}

	return nil
}

// LoadConfigValuesFromFile loads value for the options from a file and validates the inputs
func (gt *GT) LoadConfigValuesFromFile(file string) (*OptionValues, error) {
	fileBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	var optionValues OptionValues

	if err := yaml.UnmarshalStrict(fileBytes, &optionValues); err != nil {
		return nil, err
	}

	for _, option := range gt.Options.Base {
		val, ok := optionValues.Base[option.Name()]
		if !ok || reflect.ValueOf(val).IsZero() {
			return nil, errors.Wrap(ErrParameterNotSet, option.Name())
		}

		if err := validateFileOption(option, val, optionValues); err != nil {
			return nil, err
		}
	}

	for _, category := range gt.Options.Extensions {
		for _, option := range category.Options {
			val, ok := optionValues.Base[option.Name()]
			if !ok {
				continue
			}

			if err := validateFileOption(option, val, optionValues); err != nil {
				return nil, err
			}
		}
	}

	return &optionValues, nil
}

// nolint: gocritic // option is passed by value to improve usability when iterating a slice of options
func validateFileOption(option Option, value interface{}, optionValues OptionValues) error {
	valType := reflect.TypeOf(value)
	defaultType := reflect.TypeOf(option.Default(&optionValues))
	if valType != defaultType {
		return &ErrTypeMismatch{
			Expected: defaultType.Name(),
			Actual:   valType.Name(),
		}
	}

	if err := option.Validate(value); err != nil {
		return errors.Wrap(ErrMalformedInput, fmt.Sprintf("%s: %s", option.Name(), err.Error()))
	}

	// if it is set with shouldDisplay not set it means preconditions are not met
	if !option.ShouldDisplay(&optionValues) {
		return errors.Wrap(ErrParameterSet, option.Name())
	}

	return nil
}

func (gt *GT) LoadConfigValuesInteractively() (*OptionValues, error) {
	gt.printBanner()
	optionValues := NewOptionValues()

	for i := range gt.Options.Base {
		val := gt.loadOptionValueInteractively(&gt.Options.Base[i], optionValues)

		if val == nil {
			continue
		}

		optionValues.Base[gt.Options.Base[i].Name()] = val
	}

	gt.printProgressf("\nYou now have the option to enable additional extensions (organized in different categories)...\n\n")
	for _, category := range gt.Options.Extensions {
		gt.printCategory(category.Name)
		optionValues.Extensions[category.Name] = OptionNameToValue{}

		for i := range category.Options {
			val := gt.loadOptionValueInteractively(&category.Options[i], optionValues)

			if val == nil {
				continue
			}

			optionValues.Extensions[category.Name][category.Options[i].Name()] = val
		}
	}

	return optionValues, nil
}

func (gt *GT) loadOptionValueInteractively(option *Option, optionValues *OptionValues) interface{} {
	if !option.ShouldDisplay(optionValues) {
		return nil
	}

	val, err := gt.readOptionValue(option, optionValues)
	for err != nil {
		gt.printWarningf(err.Error())
		val, err = gt.readOptionValue(option, optionValues)
	}

	return val
}

func (gt *GT) InitNewProject(opts *NewRepositoryOptions) (err error) {
	gt.printProgressf("Generating repo folder...")

	targetDir := path.Join(opts.OutputDir, opts.OptionValues.Base["projectSlug"].(string))
	gt.printProgressf("Writing to %s...", targetDir)

	if _, err := os.Stat(targetDir); !os.IsNotExist(err) {
		return errors.Wrapf(ErrAlreadyExists, "directory %s", targetDir)
	}

	defer func() {
		if err != nil {
			// ignore error to not overwrite original error
			_ = os.RemoveAll(targetDir)
		}
	}()
	err = fs.WalkDir(gotemplate.FS, gotemplate.Key, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		pathToWrite, err := gt.executeTemplateString(path, opts.OptionValues)
		if err != nil {
			return err
		}

		pathToWrite = strings.ReplaceAll(pathToWrite, gotemplate.Key, targetDir)
		if d.IsDir() {
			return os.MkdirAll(pathToWrite, os.ModePerm)
		}

		fileBytes, err := fs.ReadFile(gotemplate.FS, path)
		if err != nil {
			return err
		}

		data, err := gt.executeTemplateString(string(fileBytes), opts.OptionValues)
		if err != nil {
			return err
		}

		return os.WriteFile(pathToWrite, []byte(data), os.ModePerm)
	})
	if err != nil {
		return err
	}

	gt.printProgressf("Removing obsolete files of unused integrations...")
	if err := postHook(gt.Options, opts.OptionValues, targetDir); err != nil {
		return err
	}

	gt.printProgressf("Initializing git and Go modules...")
	if err := initRepo(targetDir, opts.OptionValues.Base["moduleName"].(string)); err != nil {
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

	if err := goModInit.Run(); err != nil {
		return err
	}

	goModTidy := exec.Command("go", "mod", "tidy")
	goModTidy.Dir = targetDir

	return goModTidy.Run()
}

func postHook(options *Options, optionValues *OptionValues, targetDir string) error {
	for _, option := range options.Base {
		optionValue, ok := optionValues.Base[option.Name()]
		if !ok {
			return nil
		}

		if err := option.PostHook(optionValue, optionValues, targetDir); err != nil {
			return err
		}
	}

	for _, category := range options.Extensions {
		for _, option := range category.Options {
			optionValue, ok := optionValues.Extensions[category.Name][option.Name()]
			if !ok {
				return nil
			}

			if err := option.PostHook(optionValue, optionValues, targetDir); err != nil {
				return err
			}
		}
	}

	return nil
}

// readOptionValue reads a value for an option from the cli.
func (gt *GT) readOptionValue(opt *Option, optionValues *OptionValues) (interface{}, error) {
	gt.printOption(opt, optionValues)
	defer fmt.Fprintln(gt.Out)

	s, err := gt.readStdin()
	if err != nil {
		return nil, err
	}

	defaultVal := opt.Default(optionValues)

	var returnVal interface{}

	// TODO: cleanup somehow
	if s == "" {
		returnVal = defaultVal
	} else {
		switch defaultVal.(type) {
		case string:
			returnVal = s
		case bool:
			boolVal, err := strconv.ParseBool(s)
			if err != nil {
				return nil, err
			}
			returnVal = boolVal
		case int:
			intVal, err := strconv.Atoi(s)
			if err != nil {
				return nil, err
			}
			returnVal = intVal
		default:
			panic("unsupported type")
		}
	}

	if err := opt.Validate(returnVal); err != nil {
		gt.printf("\n")
		gt.printWarningf("Validation failed: %s", err.Error())
		return gt.readOptionValue(opt, optionValues)
	}

	return returnVal, nil
}

func (gt *GT) readStdin() (string, error) {
	if ok := gt.InScanner.Scan(); !ok {
		return "", gt.InScanner.Err()
	}

	return strings.TrimSpace(gt.InScanner.Text()), nil
}

// executeTemplateString executes the template in input str with the default p.FuncMap and valueMap as data.
func (gt *GT) executeTemplateString(str string, optionValues *OptionValues) (string, error) {
	tmpl, err := template.New("").Funcs(gt.FuncMap).Parse(str)
	if err != nil {
		return "", err
	}

	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, optionValues); err != nil {
		return "", err
	}

	return buffer.String(), nil
}