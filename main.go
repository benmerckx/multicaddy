package main

import (
	"os"
	"fmt"
	"log"
	"io"
	"strings"
	"path/filepath"
	"io/ioutil"
	//"flag"
	"github.com/mholt/caddy/caddyfile"
	"github.com/fsnotify/fsnotify"
)

// -- Main

func main() {
	var args args = os.Args[1:]
	var multi MultiCaddy
	lastConfig := ""
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	process := func() {
		fmt.Println("process")
		newConfig := multi.Config(watcher)
		if newConfig != lastConfig {
			ioutil.WriteFile("caddy.txt", []byte(newConfig), 0644)
			lastConfig = newConfig
		}
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "-remap" {
			args.remove(i)
			multi.Add(remap{
				path: args.shift(i), 
				pattern: args.shift(i), 
				default_file: args.shift(i),
			})
			i -= 1
		}
	}

	done := make(chan bool)
	process()
	go func() {
		for {
			select {
			case e := <-watcher.Events:
				file := e.Name
				stat, err := os.Stat(file)
				if err == nil {
					if stat.IsDir() || stat.Name() == "Caddyfile" {
						process()
					}
				}
			case err := <-watcher.Errors:
            	fmt.Println("error:", err)
			}
		}
	}()
	<-done
}

// -- MultiCaddy

type MultiCaddy []remap

func (this *MultiCaddy) Add(remap remap) {
	*this = append(*this, remap)
}

func (this *MultiCaddy) Config(watcher *fsnotify.Watcher) string {
	config := ""
	for _, remap := range *this {
		config += remap.config(watcher)
	}
	return config
}

// -- Remap

type remap struct {
    path string
    pattern string
    default_file string
}

func (this *remap) getPaths() []string {
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

func (this *remap) config(watcher *fsnotify.Watcher) string {
	config := ""
	for _, path := range this.getPaths() {
		files, _ := ioutil.ReadDir(path)
		for _, file := range files {
			if file.IsDir() {
				location := filepath.Join(path, file.Name())
				caddyFile := filepath.Join(location, "Caddyfile")
				handle, err := os.Open(caddyFile)
				if err == nil {
					config += this.remapConfig(handle, location)
					watcher.Add(caddyFile)
					//watcher.Remove(location)
				} else {
					// Add default file
					if this.default_file != "" {
						handle, err := os.Open(this.default_file)
						if err == nil {
							config += this.remapConfig(handle, location)
						}
					}
					watcher.Add(location)
					//watcher.Remove(caddyFile)
				}
			}
		}
		watcher.Add(path)
	}
	return config
}

func (this *remap) remapConfig(buffer io.Reader, location string) string {
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
		if len(root) > 1 {
			rootPath = filepath.Join(rootPath, root[1].Text)
		}
		block.Tokens["root"] = []caddyfile.Token{caddyfile.Token{"", -1, "root"}, caddyfile.Token{"", -1, rootPath}}
		
		// Append directives
		line := 0
		for _, directive := range block.Tokens {
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
		(*a).remove(i)
		return value
	}
}