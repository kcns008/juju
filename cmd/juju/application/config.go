// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.
package application

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/gnuflag"

	"github.com/juju/juju/api/application"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/cmd/juju/block"
	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/cmd/output"
	"github.com/juju/utils/keyvalues"
)

const (
	configSummary = `Gets, sets, or resets configuration for a deployed application.`
	configDetails = `By default, all configuration (keys, values, metadata) for the application are
displayed if a key is not specified.

Output includes the name of the charm used to deploy the application and a
listing of the application-specific configuration settings.
See ` + "`juju status`" + ` for application names.

Examples:
    juju config <application-name>
	juju config --format=json <application-name>
    juju config <application-name> wait-timeout
	juju config <application-name> key=value key2=value2
	juju config <application-name> --reset key key2
	juju config <application-name> --file path/to/config.yaml

See also:
    deploy
    status
`
	maxValueSize = 5242880 // Max size for a config file.
)

// NewConfigCommand returns a command used to get application attributes.
func NewConfigCommand() cmd.Command {
	return modelcmd.Wrap(&configCommand{})
}

type attributes map[string]string

// configCommand get, sets, and resets configuration values of an application.
type configCommand struct {
	api configCommandAPI
	modelcmd.ModelCommandBase
	out cmd.Output

	action          func(*cmd.Context) error // get, set, or reset action set in  Init
	applicationName string
	configFile      cmd.FileVar
	useFile         bool
	keys            []string
	reset           bool
	values          attributes
}

// configCommandAPI is an interface to allow passing in a fake implementation under test.
type configCommandAPI interface {
	Close() error
	Update(args params.ApplicationUpdate) error
	Get(application string) (*params.ApplicationGetResults, error)
	Set(application string, options map[string]string) error
	Unset(application string, options []string) error
}

// Info is part of the cmd.Command interface.
func (c *configCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "config",
		Args:    "<application name> [<attribute-key>[<=attribute-value>] ...]",
		Purpose: configSummary,
		Doc:     configDetails,
	}
}

// SetFlags is part of the cmd.Command interface.
func (c *configCommand) SetFlags(f *gnuflag.FlagSet) {
	c.ModelCommandBase.SetFlags(f)
	c.out.AddFlags(f, "yaml", output.DefaultFormatters)
	f.Var(&c.configFile, "file", "path to yaml-formatted application config")
	f.BoolVar(&c.reset, "reset", false, "Reset the proveded keys to be empty")
}

// getAPI either uses the fake API set at test time or that is nil, gets a real
// API and sets that as the API.
func (c *configCommand) getAPI() (func(), error) {
	if c.api != nil {
		return func() { c.api.Close() }, nil
	}
	root, err := c.NewAPIRoot()
	if err != nil {
		return nil, errors.Trace(err)
	}
	c.api = application.NewClient(root)
	return func() { c.api.Close() }, nil
}

// Init is part of the cmd.Command interface.
func (c *configCommand) Init(args []string) error {
	if len(args) == 0 || len(strings.Split(args[0], "=")) > 1 {
		return errors.New("no application name specified")
	}
	c.applicationName = args[0]
	if c.reset {
		c.action = c.resetConfig
		return c.parseReset(args[1:])
	}
	if c.configFile.Path != "" {
		return c.parseSet(args[1:], true)
	}
	if len(args[1:]) > 0 && strings.Contains(args[1], "=") {
		return c.parseSet(args[1:], false)
	}
	return c.parseGet(args[1:])
}

// parseReset parses command line args when the --reset flag is supplied.
func (c *configCommand) parseReset(args []string) error {
	if len(args) == 0 {
		return errors.New("no configuration options specified")
	}
	c.action = c.resetConfig
	c.keys = args

	return nil
}

// parseSet parses the command line args when --file is set or if the
// positional args are key=value pairs.
func (c *configCommand) parseSet(args []string, file bool) error {
	if file && len(args) > 0 {
		return errors.New("cannot specify --file and key=value arguments simultaneously")
	}
	c.action = c.setConfig
	if file {
		c.useFile = true
		return nil
	}

	settings, err := keyvalues.Parse(args, true)
	if err != nil {
		return err
	}
	c.values = settings

	return nil
}

// parseGet parses the command line args if we aren't setting or resetting.
func (c *configCommand) parseGet(args []string) error {
	if len(args) > 1 {
		return errors.New("can only retrieve a single value, or all values")
	}
	c.action = c.getConfig
	c.keys = args
	return nil
}

// Run implements the cmd.Command interface.
func (c *configCommand) Run(ctx *cmd.Context) error {
	closer, err := c.getAPI()
	if err != nil {
		return errors.Trace(err)
	}
	defer closer()

	return c.action(ctx)
}

// resetConfig is the run action when we are resetting attributes.
func (c *configCommand) resetConfig(ctx *cmd.Context) error {
	return block.ProcessBlockedError(c.api.Unset(c.applicationName, c.keys), block.BlockChange)
}

// setConfig is the run action when we are setting new attribute values as args
// or as a file passed in.
func (c *configCommand) setConfig(ctx *cmd.Context) error {
	if c.useFile {
		b, err := c.configFile.Read(ctx)
		if err != nil {
			return err
		}
		return block.ProcessBlockedError(
			c.api.Update(
				params.ApplicationUpdate{
					ApplicationName: c.applicationName,
					SettingsYAML:    string(b)}), block.BlockChange)
	}
	settings := map[string]string{}
	for k, v := range c.values {
		//empty string is also valid as a setting value
		if v == "" {
			settings[k] = v
			continue
		}

		if v[0] != '@' {
			if !utf8.ValidString(v) {
				return errors.Errorf("value for option %q contains non-UTF-8 sequences", k)
			}
			settings[k] = v
			continue
		}
		nv, err := readValue(ctx, v[1:])
		if err != nil {
			return err
		}
		if !utf8.ValidString(nv) {
			return errors.Errorf("value for option %q contains non-UTF-8 sequences", k)
		}
		settings[k] = nv
	}

	result, err := c.api.Get(c.applicationName)
	if err != nil {
		return err
	}

	for k, v := range settings {
		configValue := result.Config[k]

		configValueMap, ok := configValue.(map[string]interface{})
		if ok {
			// convert the value to string and compare
			if fmt.Sprintf("%v", configValueMap["value"]) == v {
				logger.Warningf("the configuration setting %q already has the value %q", k, v)
			}
		}
	}

	return block.ProcessBlockedError(c.api.Set(c.applicationName, settings), block.BlockChange)
}

// getConfig is the run action to return one or all configuration values.
func (c *configCommand) getConfig(ctx *cmd.Context) error {
	results, err := c.api.Get(c.applicationName)
	if err != nil {
		return err
	}
	if len(c.keys) == 1 {
		key := c.keys[0]
		info, found := results.Config[key].(map[string]interface{})
		if !found {
			return errors.Errorf("key %q not found in %q application settings.", key, c.applicationName)
		}
		out := &bytes.Buffer{}
		err := cmd.FormatYaml(out, info["value"])
		if err != nil {
			return err
		}
		fmt.Fprint(ctx.Stdout, out.String())
		return nil
	}

	resultsMap := map[string]interface{}{
		"application": results.Application,
		"charm":       results.Charm,
		"settings":    results.Config,
	}
	return c.out.Write(ctx, resultsMap)
}

// readValue reads the value of an option out of the named file.
// An empty content is valid, like in parsing the options. The upper
// size is 5M.
func readValue(ctx *cmd.Context, filename string) (string, error) {
	absFilename := ctx.AbsPath(filename)
	fi, err := os.Stat(absFilename)
	if err != nil {
		return "", errors.Errorf("cannot read option from file %q: %v", filename, err)
	}
	if fi.Size() > maxValueSize {
		return "", errors.Errorf("size of option file is larger than 5M")
	}
	content, err := ioutil.ReadFile(ctx.AbsPath(filename))
	if err != nil {
		return "", errors.Errorf("cannot read option from file %q: %v", filename, err)
	}
	return string(content), nil
}
