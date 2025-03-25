// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package probe holds probe related files
package probe

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DataDog/datadog-agent/comp/etw"
	etwimpl "github.com/DataDog/datadog-agent/comp/etw/impl"
	"github.com/DataDog/datadog-agent/pkg/security/seclog"
	"github.com/DataDog/datadog-agent/pkg/util/winutil"

	"golang.org/x/sys/windows"
)

// Microsoft-Windows-Kernel-File - https://github.com/repnz/etw-providers-docs/blob/master/Manifests-Win10-17134/Microsoft-Windows-Kernel-File.xml
const (
	idNameCreate       = uint16(10)
	idNameDelete       = uint16(11)
	idCreate           = uint16(12)
	idCleanup          = uint16(13)
	idClose            = uint16(14)
	idRead             = uint16(15)
	idWrite            = uint16(16)
	idSetInformation   = uint16(17)
	idSetDelete        = uint16(18)
	idRename           = uint16(19)
	idDirEnum          = uint16(20)
	idFlush            = uint16(21)
	idQueryInformation = uint16(22)
	idFSCTL            = uint16(23)
	idOperationEnd     = uint16(24)
	idDirNotify        = uint16(25)
	idDeletePath       = uint16(26)
	idRenamePath       = uint16(27)
	idSetLinkPath      = uint16(28)
	idRename29         = uint16(29)
	idCreateNewFile    = uint16(30)
)

type fileObjectPointer uint64

var (
	errDiscardedPath = errors.New("discarded path")
	errReadNoPath    = errors.New("read with no path")
)

/*
<template tid="CreateArgs">

	<data name="Irp" inType="win:Pointer"/>
	<data name="ThreadId" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="CreateOptions" inType="win:UInt32"/>
	<data name="CreateAttributes" inType="win:UInt32"/>
	<data name="ShareAccess" inType="win:UInt32"/>
	<data name="FileName" inType="win:UnicodeString"/>

</template>
<template tid="CreateArgs">

	<data name="Irp" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="IssuingThreadId" inType="win:UInt32"/>
	<data name="CreateOptions" inType="win:UInt32"/>
	<data name="CreateAttributes" inType="win:UInt32"/>
	<data name="ShareAccess" inType="win:UInt32"/>
	<data name="FileName" inType="win:UnicodeString"/>

</template>
*/
type kfCreateArgs struct {
	etw.DDEventHeader
	irp              uint64            // actually a pointer
	fileObject       fileObjectPointer // pointer
	threadID         uint64            // actually a pointer
	createOptions    uint32
	createAttributes uint32
	shareAccess      uint32
	fileName         string
	userFileName     string
}

/*
 * these constants are defined in the windows driver kit (wdm.h).  Copied
 * here because the correspond to the createOptions field
 */
const (
	kernelDisposition_FILE_SUPERSEDE           = uint32(0x00000000) // nolint:unused,revive
	kernelDisposition_FILE_OPEN                = uint32(0x00000001) // nolint:unused,revive
	kernelDisposition_FILE_CREATE              = uint32(0x00000002) // nolint:unused,revive
	kernelDisposition_FILE_OPEN_IF             = uint32(0x00000003) // nolint:unused,revive
	kernelDisposition_FILE_OVERWRITE           = uint32(0x00000004) // nolint:unused,revive
	kernelDisposition_FILE_OVERWRITE_IF        = uint32(0x00000005) // nolint:unused,revive
	kernelDisposition_FILE_MAXIMUM_DISPOSITION = uint32(0x00000005) // nolint:unused,revive
)

const (
	kernelCreateOpts_FILE_DIRECTORY_FILE            = uint32(0x00000001) // nolint:unused,revive
	kernelCreateOpts_FILE_WRITE_THROUGH             = uint32(0x00000002) // nolint:unused,revive
	kernelCreateOpts_FILE_SEQUENTIAL_ONLY           = uint32(0x00000004) // nolint:unused,revive
	kernelCreateOpts_FILE_NO_INTERMEDIATE_BUFFERING = uint32(0x00000008) // nolint:unused,revive

	kernelCreateOpts_FILE_SYNCHRONOUS_IO_ALERT    = uint32(0x00000010) // nolint:unused,revive
	kernelCreateOpts_FILE_SYNCHRONOUS_IO_NONALERT = uint32(0x00000020) // nolint:unused,revive
	kernelCreateOpts_FILE_NON_DIRECTORY_FILE      = uint32(0x00000040) // nolint:unused,revive
	kernelCreateOpts_FILE_CREATE_TREE_CONNECTION  = uint32(0x00000080) // nolint:unused,revive

	kernelCreateOpts_FILE_COMPLETE_IF_OPLOCKED = uint32(0x00000100) // nolint:unused,revive
	kernelCreateOpts_FILE_NO_EA_KNOWLEDGE      = uint32(0x00000200) // nolint:unused,revive
	kernelCreateOpts_FILE_OPEN_REMOTE_INSTANCE = uint32(0x00000400) // nolint:unused,revive
	kernelCreateOpts_FILE_RANDOM_ACCESS        = uint32(0x00000800) // nolint:unused,revive

	kernelCreateOpts_FILE_DELETE_ON_CLOSE        = uint32(0x00001000) // nolint:unused,revive
	kernelCreateOpts_FILE_OPEN_BY_FILE_ID        = uint32(0x00002000) // nolint:unused,revive
	kernelCreateOpts_FILE_OPEN_FOR_BACKUP_INTENT = uint32(0x00004000) // nolint:unused,revive
	kernelCreateOpts_FILE_NO_COMPRESSION         = uint32(0x00008000) // nolint:unused,revive
)

/*
The Parameters.Create.Options member is a ULONG value that describes the options that are used

	when opening the handle. The high 8 bits correspond to the value of the CreateDisposition parameter
	of ZwCreateFile, and the low 24 bits correspond to the value of the CreateOptions parameter of ZwCreateFile.

The Parameters.Create.ShareAccess member is a USHORT value that describes the type of share access.
This value corresponds to the value of the ShareAccess parameter of ZwCreateFile.

The Parameters.Create.FileAttributes and Parameters.Create.EaLength members are reserved for use

	by file systems and file system filter drivers. For more information, see the IRP_MJ_CREATE topic in
	the Installable File System (IFS) documentation.
*/
func (wp *WindowsProbe) parsekfCreateArgs(e *etw.DDEventRecord) (*kfCreateArgs, error) {
	ca := &kfCreateArgs{
		DDEventHeader: e.EventHeader,
	}
	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		ca.irp = data.GetUint64(0)
		ca.threadID = data.GetUint64(8)
		ca.fileObject = fileObjectPointer(data.GetUint64(16))
		ca.createOptions = data.GetUint32(24)
		ca.createAttributes = data.GetUint32(28)
		ca.shareAccess = data.GetUint32(32)

		ca.fileName, _, _, _ = data.ParseUnicodeString(36)
	} else if e.EventHeader.EventDescriptor.Version == 1 {
		ca.irp = data.GetUint64(0)
		ca.fileObject = fileObjectPointer(data.GetUint64(8))
		ca.threadID = uint64(data.GetUint32(16))
		ca.createOptions = data.GetUint32(20)
		ca.createAttributes = data.GetUint32(24)
		ca.shareAccess = data.GetUint32(28)

		ca.fileName, _, _, _ = data.ParseUnicodeString(32)
	} else {
		return nil, fmt.Errorf("unknown version %v", e.EventHeader.EventDescriptor.Version)
	}

	// invalidate the path resolver entry
	wp.filePathResolver.Remove(ca.fileObject)

	// not amazing to double compute the basename.
	basename := filepath.Base(ca.fileName)

	if !wp.approveFimBasename(basename) {
		wp.discardedFileHandles.Add(fileObjectPointer(ca.fileObject), struct{}{})
		wp.stats.createFileApproverRejects++
		return nil, errDiscardedPath
	}

	if _, ok := wp.discardedPaths.Get(ca.fileName); ok {
		wp.discardedFileHandles.Add(fileObjectPointer(ca.fileObject), struct{}{})
		wp.stats.fileCreateSkippedDiscardedPaths++
		return nil, errDiscardedPath
	}

	ca.userFileName = wp.mustConvertDrivePath(ca.fileName)
	if _, ok := wp.discardedUserPaths.Get(ca.userFileName); ok {
		wp.stats.fileCreateSkippedDiscardedPaths++
		return nil, errDiscardedPath
	}

	if _, ok := wp.discardedBasenames.Get(basename); ok {
		wp.discardedFileHandles.Add(fileObjectPointer(ca.fileObject), struct{}{})
		wp.stats.fileCreateSkippedDiscardedBasenames++
		return nil, errDiscardedPath
	}

	// lru is thread safe, has its own locking
	fc := fileCache{
		fileName:     ca.fileName,
		userFileName: ca.userFileName,
	}
	if wp.filePathResolver.Add(ca.fileObject, fc) {
		wp.stats.fileNameCacheEvictions++
	}
	// if we get here, we have a new file handle. Remove it from the discarder cache in case
	// we missed the close notification
	wp.discardedFileHandles.Remove(fileObjectPointer(ca.fileObject))

	return ca, nil
}

func (wp *WindowsProbe) parseKfCreateNewFileArgs(e *etw.DDEventRecord) (*kfCreateArgs, error) {
	return wp.parsekfCreateArgs(e)
}

// nolint: unused
func (ca *kfCreateArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + " PID: " + strconv.Itoa(int(ca.ProcessID)) + ", ")
	output.WriteString("Name: " + ca.fileName + ", ")
	output.WriteString("Opts: " + strconv.FormatUint(uint64(ca.createOptions), 16) + " Share: " + strconv.FormatUint(uint64(ca.shareAccess), 16) + ",")
	output.WriteString("Obj: " + strconv.FormatUint(uint64(ca.fileObject), 16))

	return output.String()
}

// nolint: unused
func (ca *kfCreateArgs) String() string {
	return ca.string("CREATE")
}

/*
<template tid="SetInformationArgs">
	<data name="Irp" inType="win:Pointer"/>
	<data name="ThreadId" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>
	<data name="ExtraInformation" inType="win:Pointer"/>
	<data name="InfoClass" inType="win:UInt32"/>
</template>
<template tid="SetInformationArgs_V1">
	<data name="Irp" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>
	<data name="ExtraInformation" inType="win:Pointer"/>
	<data name="IssuingThreadId" inType="win:UInt32"/>
	<data name="InfoClass" inType="win:UInt32"/>
</template>
*/
// nolint: unused
type kfSetInformationArgs struct {
	etw.DDEventHeader
	irp          uint64
	threadID     uint64
	fileObject   fileObjectPointer
	fileKey      uint64
	extraInfo    uint64
	infoClass    uint32
	fileName     string
	userFileName string
}
type kfSetDeleteArgs kfSetInformationArgs
type kfRenameArgs kfSetInformationArgs
type kfFsctlArgs kfSetInformationArgs

// nolint: unused
func (wp *WindowsProbe) parseKfSetInformationArgs(e *etw.DDEventRecord) (*kfSetInformationArgs, error) {
	sia := &kfSetInformationArgs{
		DDEventHeader: e.EventHeader,
	}

	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		sia.irp = data.GetUint64(0)
		sia.threadID = data.GetUint64(8)
		sia.fileObject = fileObjectPointer(data.GetUint64(16))
		sia.fileKey = data.GetUint64(24)
		sia.extraInfo = data.GetUint64(32)
		sia.infoClass = data.GetUint32(40)
	} else if e.EventHeader.EventDescriptor.Version == 1 {
		sia.irp = data.GetUint64(0)
		sia.fileObject = fileObjectPointer(data.GetUint64(8))
		sia.fileKey = data.GetUint64(16)
		sia.extraInfo = data.GetUint64(24)
		sia.threadID = uint64(data.GetUint32(32))
		sia.infoClass = data.GetUint32(36)
	} else {
		return nil, fmt.Errorf("unknown version number %v", e.EventHeader.EventDescriptor.Version)
	}

	if _, ok := wp.discardedFileHandles.Get(fileObjectPointer(sia.fileObject)); ok {
		return nil, errDiscardedPath
	}
	// lru is thread safe, has its own locking
	if s, ok := wp.filePathResolver.Get(fileObjectPointer(sia.fileObject)); ok {
		sia.fileName = s.fileName
		sia.userFileName = s.userFileName
	}

	return sia, nil
}

// nolint: unused
func (sia *kfSetInformationArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + " TID: " + strconv.Itoa(int(sia.threadID)) + ", ")
	output.WriteString("Name: " + sia.fileName + ", ")
	output.WriteString("InfoClass: " + strconv.FormatUint(uint64(sia.infoClass), 16) + ", ")
	output.WriteString("Obj: " + strconv.FormatUint(uint64(sia.fileObject), 16) + ", ")
	output.WriteString("Key: " + strconv.FormatUint(uint64(sia.fileKey), 16))

	return output.String()
}

// nolint: unused
func (sia *kfSetInformationArgs) String() string {
	return sia.string("SET_INFORMATION")
}

// nolint: unused
func (wp *WindowsProbe) parseKfSetDeleteArgs(e *etw.DDEventRecord) (*kfSetDeleteArgs, error) {
	sda, err := wp.parseKfSetInformationArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfSetDeleteArgs)(sda), nil
}

// nolint: unused
func (sda *kfSetDeleteArgs) String() string {
	return (*kfSetInformationArgs)(sda).string("SET_DELETE")
}

// nolint: unused
func (wp *WindowsProbe) parseKfRenameArgs(e *etw.DDEventRecord) (*kfRenameArgs, error) {
	ra, err := wp.parseKfSetInformationArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfRenameArgs)(ra), nil
}

// nolint: unused
func (ra *kfRenameArgs) String() string {
	return (*kfSetInformationArgs)(ra).string("RENAME")
}

// nolint: unused
func (wp *WindowsProbe) parseKfFsctlArgs(e *etw.DDEventRecord) (*kfFsctlArgs, error) {
	fa, err := wp.parseKfSetInformationArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfFsctlArgs)(fa), nil
}

// nolint: unused
func (fa *kfFsctlArgs) String() string {
	return (*kfSetInformationArgs)(fa).string("FSCTL")
}

/*
<template tid="CleanupArgs">

	<data name="Irp" inType="win:Pointer"/>
	<data name="threadID" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>

</template>
<template tid="CleanupArgs_V1">

	<data name="Irp" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>
	<data name="IssuingThreadId" inType="win:UInt32"/>

</template>
*/
type kfCleanupArgs struct {
	etw.DDEventHeader
	irp          uint64
	threadID     uint64
	fileObject   fileObjectPointer
	fileKey      uint64
	fileName     string
	userFileName string
}

// nolint: unused
type kfCloseArgs kfCleanupArgs

// nolint: unused
type kfFlushArgs kfCleanupArgs

func (wp *WindowsProbe) parseKfCleanupArgs(e *etw.DDEventRecord) (*kfCleanupArgs, error) {
	ca := &kfCleanupArgs{
		DDEventHeader: e.EventHeader,
	}
	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		ca.irp = data.GetUint64(0)
		ca.threadID = data.GetUint64(8)
		ca.fileObject = fileObjectPointer(data.GetUint64(16))
		ca.fileKey = data.GetUint64(24)

	} else if e.EventHeader.EventDescriptor.Version == 1 {
		ca.irp = data.GetUint64(0)
		ca.fileObject = fileObjectPointer(data.GetUint64(8))
		ca.fileKey = data.GetUint64(16)
		ca.threadID = uint64(data.GetUint32(24))
	} else {
		return nil, fmt.Errorf("unknown version number %v", e.EventHeader.EventDescriptor.Version)
	}

	if _, ok := wp.discardedFileHandles.Get(fileObjectPointer(ca.fileObject)); ok {
		return nil, errDiscardedPath
	}
	// lru is thread safe, has its own locking
	if s, ok := wp.filePathResolver.Get(fileObjectPointer(ca.fileObject)); ok {
		ca.fileName = s.fileName
		ca.userFileName = s.userFileName
	}

	return ca, nil
}

// nolint: unused
func (wp *WindowsProbe) parseKfCloseArgs(e *etw.DDEventRecord) (*kfCloseArgs, error) {
	ca, err := wp.parseKfCleanupArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfCloseArgs)(ca), nil
}

// nolint: unused
func (wp *WindowsProbe) parseKfFlushArgs(e *etw.DDEventRecord) (*kfFlushArgs, error) {
	ca, err := wp.parseKfCleanupArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfFlushArgs)(ca), nil
}

// nolint: unused
func (ca *kfCleanupArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + ": TID: " + strconv.Itoa(int(ca.threadID)) + ", ")
	output.WriteString("Name: " + ca.fileName + ", ")
	output.WriteString("Obj: " + strconv.FormatUint(uint64(ca.fileObject), 16) + ", ")
	output.WriteString("Key: " + strconv.FormatUint(uint64(ca.fileKey), 16))
	return output.String()

}

// nolint: unused
func (ca *kfCleanupArgs) String() string {
	return ca.string("CLEANUP")
}

// nolint: unused
func (ca *kfCloseArgs) String() string {
	return (*kfCleanupArgs)(ca).string("CLOSE")
}

// nolint: unused
func (fa *kfFlushArgs) String() string {
	return (*kfCleanupArgs)(fa).string("FLUSH")
}

type kfReadArgs struct {
	etw.DDEventHeader
	ByteOffset   uint64
	irp          uint64
	threadID     uint64
	fileObject   fileObjectPointer
	fileKey      fileObjectPointer
	IOSize       uint32
	IOFlags      uint32
	extraFlags   uint32 // zero if version 0, as they're not supplied
	fileName     string
	userFileName string
}
type kfWriteArgs kfReadArgs

func (wp *WindowsProbe) parseReadkfWriteArgs(e *etw.DDEventRecord) (*kfReadArgs, error) {
	ra := &kfReadArgs{
		DDEventHeader: e.EventHeader,
	}
	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		ra.ByteOffset = data.GetUint64(0)
		ra.irp = data.GetUint64(8)
		ra.threadID = data.GetUint64(16)
		ra.fileObject = fileObjectPointer(data.GetUint64(24))
		ra.fileKey = fileObjectPointer(data.GetUint64(32))
		ra.IOSize = data.GetUint32(40)
		ra.IOFlags = data.GetUint32(44)
	} else if e.EventHeader.EventDescriptor.Version == 1 {
		ra.ByteOffset = data.GetUint64(0)
		ra.irp = data.GetUint64(8)
		ra.fileObject = fileObjectPointer(data.GetUint64(16))
		ra.fileKey = fileObjectPointer(data.GetUint64(24))
		ra.threadID = uint64(data.GetUint32(32))
		ra.IOSize = data.GetUint32(36)
		ra.IOFlags = data.GetUint32(40)
		ra.extraFlags = data.GetUint32(44)
	} else {
		return nil, fmt.Errorf("unknown version number %v", e.EventHeader.EventDescriptor.Version)
	}
	if _, ok := wp.discardedFileHandles.Get(fileObjectPointer(ra.fileObject)); ok {
		return nil, errDiscardedPath
	}
	// lru is thread safe, has its own locking
	if s, ok := wp.filePathResolver.Get(fileObjectPointer(ra.fileObject)); ok {
		ra.fileName = s.fileName
		ra.userFileName = s.userFileName
	} else {
		return nil, errReadNoPath
	}

	return ra, nil
}

// nolint: unused
func (ra *kfReadArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + ": PID: " + strconv.Itoa(int(ra.DDEventHeader.ProcessID)) + ", ")
	output.WriteString("Obj: " + strconv.FormatUint(uint64(ra.fileObject), 16) + ", ")
	output.WriteString("Key: " + strconv.FormatUint(uint64(ra.fileKey), 16) + ", ")
	output.WriteString("Name: " + ra.fileName + ", ")
	output.WriteString("Size: " + strconv.FormatUint(uint64(ra.IOSize), 16))
	return output.String()

}

// nolint: unused
func (ra *kfReadArgs) String() string {
	return ra.string("READ")
}

func (wp *WindowsProbe) parseKfWriteArgs(e *etw.DDEventRecord) (*kfWriteArgs, error) {
	wa, err := wp.parseReadkfWriteArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfWriteArgs)(wa), nil
}

func (wa *kfWriteArgs) String() string {
	return (*kfReadArgs)(wa).string("WRITE")
}

/*
<template tid="DeletePathArgs">

	<data name="Irp" inType="win:Pointer"/>
	<data name="ThreadId" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>
	<data name="ExtraInformation" inType="win:Pointer"/>
	<data name="InfoClass" inType="win:UInt32"/>
	<data name="FilePath" inType="win:UnicodeString"/>

</template>
<template tid="DeletePathArgs_V1">

	<data name="Irp" inType="win:Pointer"/>
	<data name="FileObject" inType="win:Pointer"/>
	<data name="FileKey" inType="win:Pointer"/>
	<data name="ExtraInformation" inType="win:Pointer"/>
	<data name="IssuingThreadId" inType="win:UInt32"/>
	<data name="InfoClass" inType="win:UInt32"/>
	<data name="FilePath" inType="win:UnicodeString"/>

</template>
*/
type kfDeletePathArgs struct {
	etw.DDEventHeader
	irp              uint64
	threadID         uint64
	fileObject       fileObjectPointer
	fileKey          fileObjectPointer
	extraInformation uint64
	infoClass        uint32
	filePath         string
	userFilePath     string
	oldPath          string
	oldUserPath      string
}

// nolint: unused
type kfRenamePath kfDeletePathArgs

// nolint: unused
type kfSetLinkPath kfDeletePathArgs

func (wp *WindowsProbe) parseKfDeletePathArgs(e *etw.DDEventRecord) (*kfDeletePathArgs, error) {
	dpa := &kfDeletePathArgs{
		DDEventHeader: e.EventHeader,
	}
	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		dpa.irp = data.GetUint64(0)
		dpa.threadID = data.GetUint64(8)
		dpa.fileObject = fileObjectPointer(data.GetUint64(16))
		dpa.fileKey = fileObjectPointer(data.GetUint64(24))
		dpa.extraInformation = data.GetUint64(32)
		dpa.infoClass = data.GetUint32(40)
		dpa.filePath, _, _, _ = data.ParseUnicodeString(44)
	} else if e.EventHeader.EventDescriptor.Version == 1 {
		dpa.irp = data.GetUint64(0)
		dpa.fileObject = fileObjectPointer(data.GetUint64(8))
		dpa.fileKey = fileObjectPointer(data.GetUint64(16))
		dpa.extraInformation = data.GetUint64(24)
		dpa.threadID = uint64(data.GetUint32(32))
		dpa.infoClass = data.GetUint32(36)
		dpa.filePath, _, _, _ = data.ParseUnicodeString(40)
	}
	dpa.userFilePath = wp.mustConvertDrivePath(dpa.filePath)

	if _, ok := wp.discardedFileHandles.Get(fileObjectPointer(dpa.fileObject)); ok {
		return nil, errDiscardedPath
	}
	// lru is thread safe, has its own locking
	if s, ok := wp.filePathResolver.Get(fileObjectPointer(dpa.fileObject)); ok {
		dpa.oldPath = s.fileName
		dpa.oldUserPath = s.userFileName
		// question, should we reset the filePathResolver here?
	}
	return dpa, nil
}

// nolint: unused
func (dpa *kfDeletePathArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + ": PID: " + strconv.Itoa(int(dpa.ProcessID)) + ", ")
	output.WriteString("Name: " + dpa.filePath + ", ")
	output.WriteString("Obj: " + strconv.FormatUint(uint64(dpa.fileObject), 16) + ", ")
	output.WriteString("Key: " + strconv.FormatUint(uint64(dpa.fileKey), 16))
	return output.String()

}

// nolint: unused
func (dpa *kfDeletePathArgs) String() string {
	return dpa.string("DELETE_PATH")
}

// nolint: unused
func (wp *WindowsProbe) parseKfRenamePathArgs(e *etw.DDEventRecord) (*kfRenamePath, error) {
	rpa, err := wp.parseKfDeletePathArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfRenamePath)(rpa), nil
}

// nolint: unused
func (rpa *kfRenamePath) String() string {
	return (*kfDeletePathArgs)(rpa).string("RENAME_PATH")
}

// nolint: unused
func (wp *WindowsProbe) parseKfSetLinkPathArgs(e *etw.DDEventRecord) (*kfSetLinkPath, error) {
	sla, err := wp.parseKfDeletePathArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfSetLinkPath)(sla), nil
}

// nolint: unused
func (sla *kfSetLinkPath) String() string {
	return (*kfDeletePathArgs)(sla).string("SET_LINK_PATH")
}

type kfNameCreateArgs struct {
	etw.DDEventHeader
	fileKey      fileObjectPointer
	fileName     string
	userFileName string
}

type kfNameDeleteArgs kfNameCreateArgs

func (wp *WindowsProbe) parseKfNameCreateArgs(e *etw.DDEventRecord) (*kfNameCreateArgs, error) {
	ca := &kfNameCreateArgs{
		DDEventHeader: e.EventHeader,
	}
	data := etwimpl.GetUserData(e)
	if e.EventHeader.EventDescriptor.Version == 0 {
		ca.fileKey = fileObjectPointer(data.GetUint64(0))
		ca.fileName, _, _, _ = data.ParseUnicodeString(8)
	} else if e.EventHeader.EventDescriptor.Version == 1 {
		ca.fileKey = fileObjectPointer(data.GetUint64(0))
		ca.fileName, _, _, _ = data.ParseUnicodeString(8)
	} else {
		return nil, fmt.Errorf("unknown version number %v", e.EventHeader.EventDescriptor.Version)
	}
	ca.userFileName = wp.mustConvertDrivePath(ca.fileName)

	return ca, nil
}

// nolint: unused
func (ca *kfNameCreateArgs) string(t string) string {
	var output strings.Builder

	output.WriteString(t + ": Key: " + strconv.FormatUint(uint64(ca.fileKey), 16) + ", ")
	output.WriteString("Name: " + ca.fileName)
	return output.String()

}

// nolint: unused
func (ca *kfNameCreateArgs) String() string {
	return ca.string("NAME_CREATE")
}

// nolint: unused
func (nd *kfNameDeleteArgs) String() string {
	return (*kfNameCreateArgs)(nd).string("NAME_DELETE")
}

func (wp *WindowsProbe) parseKfNameDeleteArgs(e *etw.DDEventRecord) (*kfNameDeleteArgs, error) {
	ca, err := wp.parseKfNameCreateArgs(e)
	if err != nil {
		return nil, err
	}
	return (*kfNameDeleteArgs)(ca), nil
}

// nolint: unused
func (wp *WindowsProbe) convertDrivePath(devicefilename string) (string, error) {
	// filepath doesn't seem to like the \Device\HarddiskVolume1 format
	pathchunks := strings.SplitN(devicefilename, "\\", 4)
	if len(pathchunks) > 2 {
		if strings.EqualFold(pathchunks[1], "device") {
			// first try a direct match, to avoid the `strings.ToLower` call
			replaced, ok := wp.volumeMap[pathchunks[2]]
			if !ok {
				// then try a case insensitive match
				replaced = wp.volumeMap[strings.ToLower(pathchunks[2])]
			}
			pathchunks[2] = replaced
			return filepath.Join(pathchunks[2:]...), nil
		}
	}
	return "", fmt.Errorf("Unable to parse path %v", devicefilename)
}

func (wp *WindowsProbe) mustConvertDrivePath(devicefilename string) string {
	userPath, err := wp.convertDrivePath(devicefilename)
	if err != nil {
		seclog.Errorf("failed to convert drive path: %v", err)
		return devicefilename
	}
	return userPath
}

func (wp *WindowsProbe) initializeVolumeMap() error {

	buf := make([]uint16, 1024)
	bufferLength := uint32(len(buf))

	_, err := windows.GetLogicalDriveStrings(bufferLength, &buf[0])
	if err != nil {
		return err
	}
	drives := winutil.ConvertWindowsStringList(buf)
	for _, drive := range drives {
		t := windows.GetDriveType(windows.StringToUTF16Ptr(drive[:3]))
		/*
			DRIVE_UNKNOWN
			0
			The drive type cannot be determined.
			DRIVE_NO_ROOT_DIR
			1
			The root path is invalid; for example, there is no volume mounted at the specified path.
			DRIVE_REMOVABLE
			2
			The drive has removable media; for example, a floppy drive, thumb drive, or flash card reader.
			DRIVE_FIXED
			3
			The drive has fixed media; for example, a hard disk drive or flash drive.
			DRIVE_REMOTE
			4
			The drive is a remote (network) drive.
			DRIVE_CDROM
			5
			The drive is a CD-ROM drive.
			DRIVE_RAMDISK
			6
			The drive is a RAM disk.
		*/
		if t == windows.DRIVE_FIXED {
			volpath := make([]uint16, 1024)
			vollen := uint32(len(volpath))
			_, err = windows.QueryDosDevice(windows.StringToUTF16Ptr(drive[:2]), &volpath[0], vollen)
			if err == nil {
				devname := windows.UTF16PtrToString(&volpath[0])
				paths := strings.Split(devname, "\\") // apparently, filepath.split doesn't like volume names

				if len(paths) > 2 {
					// the \Device leads to the first entry being empty
					if strings.EqualFold(paths[1], "device") {
						device := paths[2]
						wp.volumeMap[device] = drive                  // device as-is for direct match
						wp.volumeMap[strings.ToLower(device)] = drive // lower case for slower fallback
					}
				}
			}
		}
	}
	return nil
}
