// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package usm

import (
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/valyala/fastjson"

	"github.com/DataDog/datadog-agent/pkg/util/log"
)

type nodeDetector struct {
	ctx DetectionContext
}

func newNodeDetector(ctx DetectionContext) detector {
	return &nodeDetector{ctx: ctx}
}

func isJs(filepath string) bool {
	ext := strings.ToLower(path.Ext(filepath))
	return ext == ".js" || ext == ".mjs" || ext == ".cjs"
}

func (n nodeDetector) detect(args []string) (ServiceMetadata, bool) {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			if a == "-r" || a == "--require" {
				// next arg can be a js file but not the entry point. skip it
				skipNext = !strings.ContainsRune(a, '=') // in this case the value is already in this arg
				continue
			}
		} else {
			absFile := n.ctx.resolveWorkingDirRelativePath(path.Clean(a))
			entryPoint := ""
			if isJs(a) {
				entryPoint = absFile
			} else if target, err := ReadlinkFS(n.ctx.fs, absFile); err == nil {
				if !isJs(target) {
					continue
				}

				entryPoint = abs(target, filepath.Dir(absFile))
			} else {
				continue
			}

			if _, err := fs.Stat(n.ctx.fs, absFile); err == nil {
				value, ok := n.findNameFromNearestPackageJSON(entryPoint)
				if ok {
					return NewServiceMetadata(value, Nodejs), true
				}

				// We couldn't find a package.json, fall back to the script/link
				// name since it should be better than just using "node".
				base := filepath.Base(absFile)
				name := strings.TrimSuffix(base, path.Ext(base))
				return NewServiceMetadata(name, CommandLine), true
			}
		}
	}
	return ServiceMetadata{}, false
}

// FindNameFromNearestPackageJSON finds the package.json walking up from the abspath.
// if a package.json is found, returns the value of the field name if declared
func (n nodeDetector) findNameFromNearestPackageJSON(absFilePath string) (string, bool) {
	var (
		value           string
		currentFilePath string
		ok              bool
	)

	current := path.Dir(absFilePath)
	up := path.Dir(current)

	for {
		currentFilePath = path.Join(current, "package.json")
		value, ok = n.maybeExtractServiceName(currentFilePath)
		if ok || current == up {
			break
		}

		current = up
		up = path.Dir(current)
	}

	foundServiceName := ok && len(value) > 0
	if foundServiceName {
		// Save package.json path for the instrumentation detector to use.
		n.ctx.ContextMap[NodePackageJSONPath] = currentFilePath
		n.ctx.ContextMap[ServiceSubFS] = n.ctx.fs
	}

	return value, foundServiceName
}

// maybeExtractServiceName return true if a package.json has been found and eventually the value of its name field inside.
func (n nodeDetector) maybeExtractServiceName(filename string) (string, bool) {
	// using a limit reader won't be useful here because we cannot parse incomplete json
	// Hence it's better to check against the file size and avoid to allocate memory for a non-parseable content
	file, err := n.ctx.fs.Open(filename)
	if err != nil {
		return "", false
	}
	defer file.Close()
	reader, err := SizeVerifiedReader(file)
	if err != nil {
		log.Debugf("skipping package.js (%q). Err: %v", filename, err)
		return "", true // stops here
	}
	bytes, err := io.ReadAll(reader)
	if err != nil {
		log.Debugf("unable to read a package.js file (%q). Err: %v", filename, err)
		return "", true
	}
	value, err := fastjson.ParseBytes(bytes)
	if err != nil {
		log.Debugf("unable to parse a package.js (%q) file. Err: %v", filename, err)
		return "", true
	}
	return string(value.GetStringBytes("name")), true
}
