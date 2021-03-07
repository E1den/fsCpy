package main

import (
	"container/list"
	"flag"
	"fmt"
	"golang.org/x/sys/windows/registry"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

type args struct {
	copy      bool
	paste     bool
	verbose   bool
	copyPath  string
	pastePath string
	install   bool
}

type props struct {
	to   string
	from string
}

type atomicInt int32

func (c *atomicInt) inc() int32 {
	return atomic.AddInt32((*int32)(c), 1)
}
func (c *atomicInt) dec() int32 {
	return atomic.AddInt32((*int32)(c), -1)
}

func (c *atomicInt) get() int32 {
	return atomic.LoadInt32((*int32)(c))
}

var count atomicInt = 0

const BufferSize = 128_000 //64k
var MaxJobs = int32(runtime.NumCPU()) * 4

func parseArgs() args {
	current := args{copy: false, paste: false, copyPath: "nil", pastePath: "nil", verbose: false}
	verbose := flag.Bool("verbose", false, "Sets verbose")
	copy := flag.String("copy", "nil", "Copies a file")
	target := flag.String("paste", "nil", "Pastes a file")
	install := flag.Bool("install", false, "Installs handlers")

	flag.Parse()

	current.copy = (*copy) != "nil"
	current.copyPath = *copy

	current.paste = (*target) != "nil"
	current.pastePath = *target

	current.verbose = *verbose

	current.install = *install

	return current
}

func getKeyBase(path string) (registry.Key, string) {
	var base registry.Key
	if strings.HasPrefix(path, "HKEY_CLASSES_ROOT") {
		base = registry.CLASSES_ROOT
		path = path[len("HKEY_CLASSES_ROOT")+1:]
	} else if strings.HasPrefix(path, "HKEY_CURRENT_USER") {
		base = registry.CURRENT_USER
		path = path[len("HKEY_CLASSES_USER")+1:]
	} else if strings.HasPrefix(path, "HKEY_LOCAL_MACHINE") {
		base = registry.LOCAL_MACHINE
		path = path[len("HKEY_LOCAL_MACHINE")+1:]
	} else if strings.HasPrefix(path, "HKEY_USERS") {
		base = registry.USERS
		path = path[len("HKEY_USERS")+1:]
	} else if strings.HasPrefix(path, "HKEY_CURRENT_CONFIG") {
		base = registry.CURRENT_CONFIG
		path = path[len("HKEY_CLASSES_CONFIG")+1:]
	}
	return base, path
}

func setReg(path string, key string, value string) {
	base, path := getKeyBase(path)

	k, err := registry.OpenKey(base, path, registry.SET_VALUE)
	if err != nil {
		k, _, err = registry.CreateKey(base, path, registry.ALL_ACCESS)
		if err != nil {
			log.Fatal(err)
			return
		}
	}
	if err := k.SetStringValue(key, value); err != nil {
		log.Fatal(err)
		return
	}
	if err := k.Close(); err != nil {
		log.Fatal(err)
		return
	}
}

func getReg(path string, key string) string {
	base, path := getKeyBase(path)
	k, err := registry.OpenKey(base, path, registry.QUERY_VALUE)
	if err != nil {
		log.Fatal(err)
		return ""
	}
	var val string
	if val, _, err = k.GetStringValue(key); err != nil {
		log.Fatal(err)
		return ""
	}
	if err := k.Close(); err != nil {
		log.Fatal(err)
		return ""
	}
	return val
}

func ensureMenu() {
	path := os.Args[0]
	setReg(`HKEY_CLASSES_ROOT\Directory\shell\FastCopy\command`, "", fmt.Sprint(path, ` -copy="\"%1\""`))
	if currentArgs.verbose {
		setReg(`HKEY_CLASSES_ROOT\Directory\shell\FastPaste\command`, "", fmt.Sprint(path, ` -paste="\"%1\"" -verbose`))
	} else {
		setReg(`HKEY_CLASSES_ROOT\Directory\shell\FastPaste\command`, "", fmt.Sprint(path, ` -paste="\"%1\""`))
	}

	setReg(`HKEY_CLASSES_ROOT\Directory\Background\shell\FastCopy\command`, "", fmt.Sprint(path, ` -copy="\"%v\""`))
	setReg(`HKEY_CLASSES_ROOT\Directory\Background\shell\FastCopy\command`, "NoWorkingDirectory", "")
	if currentArgs.verbose {
		setReg(`HKEY_CLASSES_ROOT\Directory\Background\shell\FastPaste\command`, "", fmt.Sprint(path, ` -paste="\"%v\"" -verbose`))
	} else {
		setReg(`HKEY_CLASSES_ROOT\Directory\Background\shell\FastPaste\command`, "", fmt.Sprint(path, ` -paste="\"%v\""`))
	}
	setReg(`HKEY_CLASSES_ROOT\Directory\Background\shell\FastPaste\command`, "NoWorkingDirectory", "")

	setReg(`HKEY_CLASSES_ROOT\*\shell\FastCopy\command`, "", fmt.Sprint(path, ` -copy="\"%1\""`))
}

func rememberCopy(path string) {
	if currentArgs.verbose {
		fmt.Println("Remembering ", path)
	}
	setReg(`HKEY_CURRENT_USER\Software\FsCpy`, "path", stripSurrounding(path))
}

func setupCopy(path string) {
	if currentArgs.verbose {
		fmt.Println("Loading path", path)
	}
	from := stripSurrounding(getReg(`HKEY_CURRENT_USER\Software\FsCpy`, "path"))
	if from == "nil" {
		return
	}
	//setReg(`HKEY_CURRENT_USER\Software\FsCpy`, "path", "nil")
	doCopy(stripSurrounding(path), from)
}

func actualCopy(to string, from string) {
	if to == from {
		count.dec()
		return
	}
	source, err := os.Open(from)
	if err != nil {
		if os.IsNotExist(err) {
			count.dec()
			return
		}
		log.Fatal(err)
		return
	}
	defer source.Close()
	target, err := os.Create(to)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer target.Close()
	buffer := make([]byte, BufferSize)

	for {
		size, err := source.Read(buffer)
		if err != nil {
			if err != io.EOF {
				log.Fatal(err)
				return
			}
			break
		}
		_, err = target.Write(buffer[:size])
		if err != nil {
			log.Fatal(err)
			return
		}
	}
	if currentArgs.verbose {
		fmt.Println("Copied ", from, " to ", to)
	}
	count.dec()
}

func IsDirectory(path string) bool {
	var (
		f   os.FileInfo
		err error
	)
	if f, err = os.Stat(path); os.IsNotExist(err) {
		return false
	}
	if err != nil {
		log.Fatal(err)
		return false
	}
	return f.Mode().IsDir()
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

func stripSurrounding(str string) string {
	str = strings.TrimLeft(str, "\"")
	str = strings.TrimRight(str, "\"")
	return str
}

func fixPath(path string, fromBase string, from string) string {
	if !IsDirectory(path) {
		return path
	}
	if fromBase == from {
		fromBase = from[:strings.LastIndex(from, "\\")]
	}
	if strings.HasSuffix(path, "\\") {
		return fmt.Sprint(path, from[len(fromBase):])
	}
	temp := fmt.Sprint(path, "\\", from[len(fromBase)+1:])
	return temp
}

func doCopy(to string, from string) {
	if currentArgs.verbose {
		fmt.Println("Copying ", from, " to ", to)
	}
	if !Exists(from) || to == from {
		return
	}

	if IsDirectory(from) {
		files := list.New()
		err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
			if path == from {
				return nil
			}
			if err != nil {
				return err
			}
			newPath := fixPath(to, from, path)
			if info.IsDir() {
				if !Exists(newPath) {
					err = os.Mkdir(newPath, info.Mode().Perm())
					if err != nil {
						log.Fatal(err)
						return nil
					}
				}
			} else {
				files.PushBack(props{to: newPath, from: path})
			}
			return nil
		})
		if err != nil {
			log.Fatal(err)
			return
		}

		for e := files.Front(); e != nil; e = e.Next() {
			count.inc()
			for count.get() > MaxJobs {
				time.Sleep(500 * time.Nanosecond)
			}
			go actualCopy(e.Value.(props).to, e.Value.(props).from)
		}
		for count.get() > 0 {
			time.Sleep(time.Millisecond)
		}
	} else {
		actualCopy(fixPath(to, from, from), from)
	}
}

var currentArgs args

func main() {
	currentArgs = parseArgs()

	if currentArgs.install {
		ensureMenu()
		fmt.Println("Installed handlers.")
	}

	start := time.Now()
	if currentArgs.copy && currentArgs.paste {
		doCopy(currentArgs.pastePath, currentArgs.copyPath)
	} else if currentArgs.copy {
		rememberCopy(currentArgs.copyPath)
	} else if currentArgs.paste {
		setupCopy(currentArgs.pastePath)
	}
	elapsed := time.Since(start)
	if currentArgs.verbose {
		fmt.Println("Done in ", elapsed)
	}
}
