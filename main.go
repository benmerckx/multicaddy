package main

import (
	"os"
	"fmt"
	"log"
	"io"
	"strings"
	"sort"
	"syscall"
	"time"
	"regexp"
	"path/filepath"
	"io/ioutil"
	"os/exec"
	"github.com/mholt/caddy/caddyfile"
	"github.com/fsnotify/fsnotify"
	"github.com/hpcloud/tail"
)

const RELOAD_SUCCESS = "[INFO] Reloading complete"
const RELOAD_FAILURE = "[ERROR] SIGUSR1"
const CADDY_FILE = "Caddyfile"
const CONFIG_FILE = "config.conf"
const LOG_FILE = "caddy.log"

// -- Main

func main() {
	var args args = os.Args[1:]
	var multi MultiCaddy
	var lastConfig string
	rewatch := make(chan bool, 1)
	log, _ := tail.TailFile(LOG_FILE, tail.Config{Follow: true, Location: &tail.SeekInfo{Whence: os.SEEK_END}})

	for i := 0; i < len(args); i++ {
		if args[i] == "-remap" {
			args.remove(i)
			multi.Add(Remap{
				path: args.shift(i), 
				pattern: args.shift(i), 
				default_file: args.shift(i),
			})
			i -= 1
		}
	}

	caddy := caddy(args)
	started := false
	run := func(config string) chan bool {
		success := make(chan bool, 1)
		if !started {
			go func() {
				caddy.Start()
				started = true
				time.Sleep(time.Second * 2)
				success <- true
			}()
		} else {
			go func() {
				for line := range log.Lines {
					switch {
						case strings.Contains(line.Text, RELOAD_SUCCESS):
							fmt.Println(line.Text)
							success <- true
							return
						case strings.Contains(line.Text, RELOAD_FAILURE):
							fmt.Println(line.Text)
							success <- false
							return
					}
				}
			}()
			caddy.Process.Signal(syscall.SIGUSR1)
		}
		return success
	}
	data, err := ioutil.ReadFile(CONFIG_FILE)
	if err == nil {
		fmt.Println("Loading previous config")
		lastConfig = string(data)
		<-run(lastConfig)
	}
	go func() {
		for {
			select {
			case <-rewatch:
				config := process(rewatch, &multi)
				if config != lastConfig {
					// Write file
					file, err := os.Create(CONFIG_FILE)
					if err == nil {
						fmt.Println("Reloading config")
						file.WriteString(config)
						// Run caddy
						success := <-run(config)
						if success {
							lastConfig = config
						} else {
							// Revert config
							file.WriteString(lastConfig)
						}
						file.Close()
					}
				}
			}
		}
	}()
	rewatch <- true
	<-make(chan bool)
}

func caddy(args []string) *exec.Cmd {
	args = append([]string{"-conf", CONFIG_FILE, "-log", LOG_FILE}, args...)
	caddy := exec.Command("caddy", args...)
    caddy.Stdout = os.Stdout
    caddy.Stderr = os.Stderr
	return caddy
}

func process(rewatch chan<- bool, multi *MultiCaddy) string {
	config, watcher := multi.CreateWatcher()
	go func() {
		defer watcher.Close()
		for {
			select {
			case e := <-watcher.Events:
				remove := e.Op&fsnotify.Remove == fsnotify.Remove
				restart := remove
				file := e.Name
				if !remove {
					stat, err := os.Stat(file)
					if err == nil {
						if stat.IsDir() || stat.Name() == CADDY_FILE {
							restart = true
						}
					}
				}
				if restart {
					rewatch <- true
					return 
				}
			}
		}
	}()
	return config
}

// -- MultiCaddy

type MultiCaddy []Remap

func (this *MultiCaddy) Add(remap Remap) {
	*this = append(*this, remap)
}

func (this *MultiCaddy) Match(path string) bool {
	for _, remap := range *this {
		if remap.Match(path) {
			return true
		}
	}
	return false
}

func (this *MultiCaddy) CreateWatcher() (string, *fsnotify.Watcher) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	return this.Config(watcher), watcher
}

func (this *MultiCaddy) Config(watcher *fsnotify.Watcher) string {
	config := ""
	for _, remap := range *this {
		config += remap.Config(watcher)
	}
	return config
}

// -- Remap

type Remap struct {
	path string
	pattern string
	default_file string
}

func (this *Remap) getPaths() []string {
	var directories []string
	abs, _ := filepath.Abs(this.path)
	matches, _ := filepath.Glob(abs)
	for _, dir := range matches {
		fileInfo, err := os.Stat(dir)
		if err == nil {
			if fileInfo.IsDir() {
				directories = append(directories, dir)
			}
		}
	}
	return directories
}

func (this *Remap) Match(path string) bool {
	matched, _ := filepath.Match(this.path, path)
	return matched
}

func (this *Remap) Config(watcher *fsnotify.Watcher) string {
	config := ""
	for _, path := range this.getPaths() {
		files, _ := ioutil.ReadDir(path)
		for _, file := range files {
			if file.IsDir() {
				match, _ := regexp.MatchString("^[a-z][a-z0-9\\.\\-]+$", file.Name())
				if (!match) {
					continue
				}
				location := filepath.Join(path, file.Name())
				caddyFile := filepath.Join(location, CADDY_FILE)
				handle, err := os.Open(caddyFile)
				if err == nil {
					config += this.remapConfig(handle, location)
					watcher.Add(caddyFile)
				} else {
					// Add default file
					if this.default_file != "" {
						handle, err := os.Open(this.default_file)
						if err == nil {
							config += this.remapConfig(handle, location)
						}
					}
					watcher.Add(location)
				}
			}
		}
		watcher.Add(path)
	}
	return config
}

func (this *Remap) remapConfig(buffer io.Reader, location string) string {
	config := ""
	blocks, _ := caddyfile.Parse("Caddyfile", buffer, nil)
	parts := strings.Split(this.pattern, ":")
	if len(parts) != 2 {
		panic ("Incorrect pattern: "+this.pattern)
	}
	dirs := strings.Split(filepath.ToSlash(location), "/")
	for _, block := range blocks {

		// Replace host in keys
		for _, key := range block.Keys {
			key = strings.Replace(key, parts[0], parts[1], 1)
			for i, dir := range dirs {
				key = strings.Replace(key, "@"+fmt.Sprintf("%v", len(dirs)-i), dir, 1)
			}
			config += "\n"+key
		}
		config += " {\n"

		// Set root in directive
		rootPath := location
		root, _ := block.Tokens["root"]
		rootLine := -1
		if len(root) > 1 {
			rootPath = filepath.Join(rootPath, root[1].Text)
			rootLine = root[0].Line
		}
		block.Tokens["root"] = []caddyfile.Token{caddyfile.Token{"", rootLine, "root"}, caddyfile.Token{"", rootLine, rootPath}}
		
		// Append directives
		line := 0
		var directives Tokens
		for _, directive := range block.Tokens {
			directives = append(directives, directive)
		}
		sort.Sort(directives)
		for _, directive := range directives {
			for _, token := range directive {
				if line != token.Line {
					config += "\n"
					line = token.Line
				}
				config += " "+token.Text
			}
		}
		config += "\n}\n"
	}
	return config
}

// -- Args

type args []string

func (a *args) remove(i int) {
	*a = append((*a)[:i], (*a)[i+1:]...)
}

func (a *args) shift(i int) string {
	if i > len(*a)-1 || (*a)[i][0:1] == "-" {
		return ""
	} else {
		value := (*a)[i]
		a.remove(i)
		return value
	}
}

// -- Tokens

type Tokens [][]caddyfile.Token

func (s Tokens) Len() int {
	return len(s)
}

func (s Tokens) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Tokens) Less(i, j int) bool {
	return s[i][0].Line < s[j][0].Line
}