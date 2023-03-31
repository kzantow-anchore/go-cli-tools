package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"

	"github.com/adrg/xdg"
	"github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/anchore/go-cli-tools/inject"
	"github.com/anchore/go-cli-tools/log"
)

func Load(c inject.Container, cmd *cobra.Command, configurations ...any) error {
	// e.g. pod.context = APPNAME_POD_CONTEXT
	v := viper.NewWithOptions(viper.EnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_")))

	return c.Invoke(func(cfg Config) error {
		return load(v, cmd, cfg.AppName, cfg.ConfigFile, configurations...)
	})
}

func LoadAt(c inject.Container, cmd *cobra.Command, path string, configuration any) error {
	// allow for nested options to be specified via environment variables
	// e.g. pod.context = APPNAME_POD_CONTEXT
	v := viper.NewWithOptions(viper.EnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_")))

	t := reflect.TypeOf(configuration)
	config := reflect.StructOf([]reflect.StructField{{
		Name: upperFirst(path),
		Type: t,
		Tag:  reflect.StructTag(fmt.Sprintf(`json:"%s" yaml:"%s" mapstructure:"%s"`, path, path, path)),
	}})

	value := reflect.New(config)
	value.Elem().Field(0).Set(reflect.ValueOf(configuration))

	return c.Invoke(func(cfg Config) error {
		return load(v, cmd, cfg.AppName, cfg.ConfigFile, value.Interface())
	})
}

func upperFirst(p string) string {
	if len(p) < 2 {
		return strings.ToUpper(p)
	}
	return strings.ToUpper(p[0:1]) + p[1:]
}

func load(v *viper.Viper, cmd *cobra.Command, appName string, configFile string, configurations ...any) error {
	for _, cfg := range configurations {
		if reflect.TypeOf(cfg).Kind() != reflect.Ptr {
			return fmt.Errorf("LoadConfig cfg parameter must be a pointer, got: %s -- %v", reflect.TypeOf(cfg).Name(), cfg)
		}
	}

	// priority order: viper.Set, flag, env, config, kv, defaults
	// flags have already been loaded into viper by command construction

	// check if user specified config; otherwise read all possible paths
	if err := loadConfig(v, appName, configFile); err != nil {
		var notFound *viper.ConfigFileNotFoundError
		if errors.As(err, &notFound) {
			log.Debugf("no config file found, using defaults")
		} else {
			return fmt.Errorf("unable to load config: %w", err)
		}
	}

	// load environment variables
	v.SetEnvPrefix(appName)
	v.AllowEmptyEnv(true)
	v.AutomaticEnv()

	if appName != "" {
		appName += "."
	}

	flags := getFlags(cmd)

	for _, cfg := range configurations {
		configureViper(v, reflect.ValueOf(cfg), flags, appName, "")

		// unmarshal fully populated viper object onto config
		err := v.Unmarshal(cfg)
		if err != nil {
			return err
		}

		// Convert all populated config options to their internal application values ex: scope string => scopeOpt source.Scope
		err = processPostConfig(cfg)
		if err != nil {
			return err
		}
	}

	return nil
}

// configureViper loads the default configuration values into the viper instance, before the config values are read and parsed
func configureViper(v *viper.Viper, value reflect.Value, flags []*pflag.Flag, appPrefix string, path string) {
	for _, flag := range flags {
		if valuesEqual(value, reflect.ValueOf(flag.Value)) {
			log.Tracef("binding: %s = %v (flag)\n", strings.ToUpper(regexp.MustCompile("[^a-zA-Z0-9]").ReplaceAllString(appPrefix+path, "_")), value.Elem().Interface())
			_ = v.BindPFlag(path, flag)
			return
		}
	}

	if value.Type().Kind() == reflect.Ptr {
		value = value.Elem()
	}

	if value.Type().Kind() != reflect.Struct {
		log.Tracef("binding: %s = %v\n", strings.ToUpper(regexp.MustCompile("[^a-zA-Z0-9]").ReplaceAllString(appPrefix+path, "_")), value.Interface())
		v.SetDefault(path, value.Interface())
		return
	}

	if path != "" {
		path += "."
	}

	// for each field in the configuration struct, see if the field implements the defaultValueLoader interface and invoke it if it does
	for i := 0; i < value.NumField(); i++ {
		fieldValue := value.Field(i)
		field := value.Type().Field(i)

		mapStructTag := field.Tag.Get("mapstructure")

		if mapStructTag == "-" {
			continue
		}

		if !field.Anonymous && mapStructTag == "" {
			log.Tracef("not binding field due to lacking mapstructure tag: %s.%s", value.Type().Name(), field.Name)
			continue
		}

		if fieldValue.Type().Kind() != reflect.Ptr {
			fieldValue = fieldValue.Addr()
		}

		configureViper(v, fieldValue, flags, appPrefix, path+mapStructTag)
	}
}

func valuesEqual(v1 reflect.Value, v2 reflect.Value) bool {
	return v2.CanConvert(v1.Type()) && v1 == v2.Convert(v1.Type())
}

func getFlags(cmd *cobra.Command) (flags []*pflag.Flag) {
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, flag)
	})
	cmd.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, flag)
	})
	return
}

func hasConfig(base string) bool {
	for _, ext := range viper.SupportedExts {
		if _, err := os.Stat(fmt.Sprintf("%s.%s", base, ext)); err != nil {
			return true
		}
	}
	return false
}

// nolint:funlen
func loadConfig(v *viper.Viper, appName string, configPath string) error {
	var err error
	// use explicitly the given user config
	if configPath != "" {
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("unable to read application config=%q : %w", configPath, err)
		}
		v.Set("config", v.ConfigFileUsed())
		// don't fall through to other options if the config path was explicitly provided
		return nil
	}

	// start searching for valid configs in order...
	// 1. look for .<appname>.yaml (in the current directory)
	confFilePath := "." + appName

	// TODO: Remove this before v1.0.0
	// See syft #1634
	v.AddConfigPath(".")
	v.SetConfigName(confFilePath)

	// check if config.yaml exists in the current directory
	// DEPRECATED: this will be removed in v1.0.0
	if _, err := os.Stat("config.yaml"); err == nil {
		log.Warn("DEPRECATED: ./config.yaml as a configuration file is deprecated and will be removed as an option in v1.0.0, please rename to .syft.yaml")
	}

	if _, err := os.Stat(confFilePath + ".yaml"); err == nil {
		if err = v.ReadInConfig(); err == nil {
			v.Set("config", v.ConfigFileUsed())
			return nil
		} else if !errors.As(err, &viper.ConfigFileNotFoundError{}) {
			return fmt.Errorf("unable to parse config=%q: %w", v.ConfigFileUsed(), err)
		}
	}

	// 2. look for .<appname>/config.yaml (in the current directory)
	v.AddConfigPath("." + appName)
	v.SetConfigName("config")
	if err = v.ReadInConfig(); err == nil {
		v.Set("config", v.ConfigFileUsed())
		return nil
	} else if !errors.As(err, &viper.ConfigFileNotFoundError{}) {
		return fmt.Errorf("unable to parse config=%q: %w", v.ConfigFileUsed(), err)
	}

	// 3. look for ~/.<appname>.yaml
	home, err := homedir.Dir()
	if err == nil {
		v.AddConfigPath(home)
		v.SetConfigName("." + appName)
		if err = v.ReadInConfig(); err == nil {
			v.Set("config", v.ConfigFileUsed())
			return nil
		} else if !errors.As(err, &viper.ConfigFileNotFoundError{}) {
			return fmt.Errorf("unable to parse config=%q: %w", v.ConfigFileUsed(), err)
		}
	}

	// 4. look for <appname>/config.yaml in xdg locations (starting with xdg home config dir, then moving upwards)
	v.SetConfigName("config")
	configPath = path.Join(xdg.ConfigHome, appName)
	v.AddConfigPath(configPath)
	for _, dir := range xdg.ConfigDirs {
		v.AddConfigPath(path.Join(dir, appName))
	}
	if err = v.ReadInConfig(); err == nil {
		v.Set("config", v.ConfigFileUsed())
		return nil
	} else if !errors.As(err, &viper.ConfigFileNotFoundError{}) {
		return fmt.Errorf("unable to parse config=%q: %w", v.ConfigFileUsed(), err)
	}
	return nil
}

func processPostConfig(obj any) error {
	value := reflect.ValueOf(obj)
	typ := value.Type()
	if typ.Kind() == reflect.Ptr {
		if p, ok := obj.(PostProcess); ok {
			// the field implements parser, call it
			if err := p.PostProcess(); err != nil {
				return err
			}
		}
		value = value.Elem()
		typ = value.Type()
	}

	if typ.Kind() != reflect.Struct {
		return nil
	}

	// parse nested config options
	// for each field in the configuration struct, see if the field implements the parser interface
	// note: the app config is a pointer, so we need to grab the elements explicitly (to traverse the address)
	for i := 0; i < value.NumField(); i++ {
		f := value.Field(i)
		ft := f.Type()
		if ft.Kind() == reflect.Ptr {
			f = f.Elem()
			ft = f.Type()
		}
		if !f.CanAddr() || ft.Kind() != reflect.Struct {
			continue
		}
		// note: since the interface method of parser is a pointer receiver we need to get the value of the field as a pointer.
		// the field implements parser, call it
		if err := processPostConfig(f.Addr().Interface()); err != nil {
			return err
		}
	}

	return nil
}