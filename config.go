package main

import (
	"os"

	"go.yaml.in/yaml/v4"
)

type Config struct {
	Title       string `yaml:"title"`
	Writer      string `yaml:"writer"`
	LanguageIso string `yaml:"language_iso"`
	FirstDate   string `yaml:"first_date"`
	Homepage    string `yaml:"homepage"`
	Summary     string `yaml:"summary"`

	Cbz struct {
		ChunkSize      int    `yaml:"chunk_size"`
		NamingTemplate string `yaml:"naming_template"`
		OutputPath     string `yaml:"output_path"`
	} `yaml:"cbz"`

	Source []Source `yaml:"source"`
}

type Source struct {
	Imgfiles struct {
		PathGlob string `yaml:"path_glob"`
		IdxRegex string `yaml:"idx_regex"`
	} `yaml:"imgfiles,omitempty"`
	Httpdirect struct {
		URLFormat string `yaml:"url_format"`
	} `yaml:"httpdirect,omitempty"`
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
