package main

import (
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

const ConfigName = "webcomic2cbz.yml"

type Config struct {
	Title       string    `yaml:"title"`
	Writer      string    `yaml:"writer"`
	LanguageIso string    `yaml:"language_iso"`
	FirstDate   time.Time `yaml:"first_date"`
	Homepage    string    `yaml:"homepage"`
	Summary     string    `yaml:"summary"`

	Cbz struct {
		ChunkSize      int    `yaml:"chunk_size"`
		NamingTemplate string `yaml:"naming_template"`
	} `yaml:"cbz"`

	Source []SourceConfig `yaml:"source"`
}

type SourceConfig struct {
	Imgfiles struct {
		BasenameGlob   string `yaml:"basename_glob"`
		BasenameFormat string `yaml:"basename_format"`
		IdxRegex       string `yaml:"idx_regex"`
	} `yaml:"imgfiles,omitempty"`

	Httpdirect struct {
		URLFormat      string           `yaml:"url_format"`
		BasenameFormat string           `yaml:"basename_format"`
		LatestRule     LatestRuleConfig `yaml:"latest_rule"`
	} `yaml:"httpdirect,omitempty"`

	Htmltemplatescan struct {
		URLFormat      string           `yaml:"url_format"`
		BasenameFormat string           `yaml:"basename_format"`
		ImgXPath       string           `yaml:"img_xpath"`
		LatestRule     LatestRuleConfig `yaml:"latest_rule"`
	} `yaml:"htmltemplatescan,omitempty"`
}

type LatestRuleConfig struct {
	HomepageRegex string `yaml:"homepage_regex"`
	HomepageXPath string `yaml:"homepage_xpath"`
}

func ParseConfig(path string) (Config, error) {
	// Read the file
	bytes, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	// Unmarshal
	var config Config
	err = yaml.Unmarshal(bytes, &config)
	if err != nil {
		return Config{}, err
	}

	return config, nil
}
