/*
Copyright 2013 Tamás Gulácsi

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package oracle

/*


//#include <stdio.h>
#include <oci.h>
#include <ociap.h>
#include "version.h"

char* AttrGetName(const dvoid *mypard,
				  ub4 parType,
				  ub4 key,
                  OCIError *errhp,
                  sword *status,
                  ub4 *nameLen) {
	char *name;

	*status = OCIAttrGet(mypard, parType,
       (dvoid*)&name, nameLen,
       key, errhp);
    //fprintf(stderr, "%d %s\n", *nameLen, (char*)name);
    return name;
}
*/
import "C"

import (
	"bytes"
	"fmt"
	"unsafe"

	"gopkg.in/errgo.v1"
	"gopkg.in/inconshreveable/log15.v2"
)

// Log is discarded by default. Use Log.SetHandler to set.
var (
	// Set it to something like
	// Log.SetHandler(log15.CallerStackHandler("%v", log15.StderrHandler))
	// if you want to see the call stack, too.
	Log = log15.New("lib", "goracle")
)

// OracleVersionHex returns the major.minor version as one number,
// ready to compare with OracleVersion.
func OracleVersionHex(major, minor int) int {
	return ((major << 8) | minor)
}

const (
	// OracleVersion is the OCI version
	OracleVersion = C.ORACLE_VERSION_HEX
	// OracleV10_1 is version 10 rev 1 (begin of 4GB LOBs)
	OracleV10_1 = ((10 << 8) | 1)
	// OracleV12_1 is version 12 rev 1 (begin of 32kB strings)
	OracleV12_1 = ((12 << 8) | 1)
)

func init() {
	Log.SetHandler(log15.DiscardHandler())
}

// Environment holds handles for the database environment
type Environment struct {
	// unexported fields
	handle                       *C.OCIEnv
	errorHandle                  *C.OCIError
	MaxBytesPerCharacter         uint
	FixedWidth                   bool
	Encoding, Nencoding          string
	maxStringBytes               uint
	numberToStringFormatBuffer   []byte
	numberFromStringFormatBuffer []byte
	nlsNumericCharactersBuffer   []byte
}

// maximum number of characters/bytes applicable to strings/binaries
const (
	MaxStringChars = C.MAX_BINARY_BYTES
	MaxBinaryBytes = C.MAX_BINARY_BYTES
)

// CsIDAl32UTF8 holds the charaterset ID for UTF8
var CsIDAl32UTF8 C.ub2

// NewEnvironment creates and initializes a new environment object
func NewEnvironment() (*Environment, error) {
	var err error

	// create a new object for the Oracle environment
	env := &Environment{
		FixedWidth:                   false,
		MaxBytesPerCharacter:         4,
		maxStringBytes:               MaxStringChars,
		numberToStringFormatBuffer:   []byte("TM9"),
		numberFromStringFormatBuffer: []byte("999999999999999999999999999999999999999999999999999999999999999"),
		nlsNumericCharactersBuffer:   []byte("NLS_NUMERIC_CHARACTERS='.,'"),
	}

	if CsIDAl32UTF8 == 0 {
		// create the new environment handle
		if err = checkStatus(
			C.OCIEnvNlsCreate(&env.handle, C.OCI_DEFAULT|C.OCI_THREADED,
				nil, nil, nil, nil, 0, nil,
				0, 0),
			false,
		); err != nil { //, C.ub2(873), 0),
			setErrAt(err, "Unable to acquire Oracle environment handle")
			return nil, err
		}
		buffer := []byte("AL32UTF8\000")
		CsIDAl32UTF8 = C.OCINlsCharSetNameToId(unsafe.Pointer(env.handle),
			(*C.oratext)(&buffer[0]))
		C.OCIHandleFree(unsafe.Pointer(&env.handle), C.OCI_HTYPE_ENV)
		// Log.Debug("Env", "csid", CsIDAl32UTF8)
	}
	if err = checkStatus(
		C.OCIEnvNlsCreate(&env.handle, C.OCI_DEFAULT|C.OCI_THREADED,
			nil, nil, nil, nil, 0, nil,
			CsIDAl32UTF8, CsIDAl32UTF8),
		false,
	); err != nil {
		setErrAt(err, "Unable to acquire Oracle environment handle with AL32UTF8 charset")
		return nil, err
	}
	// Log.Debug("Env", "env", env.handle, "error", err)

	// create the error handle
	if err = ociHandleAlloc(unsafe.Pointer(env.handle),
		C.OCI_HTYPE_ERROR, (*unsafe.Pointer)(unsafe.Pointer(&env.errorHandle)),
		"env.errorHandle",
	); err != nil || env.handle == nil {
		return nil, err
	}

	var sb4 C.sb4
	// acquire max bytes per character
	if err = env.CheckStatus(C.OCINlsNumericInfoGet(unsafe.Pointer(env.handle),
		env.errorHandle, &sb4, C.OCI_NLS_CHARSET_MAXBYTESZ),
		"Environment_New(): get max bytes per character",
	); err != nil {
		return nil, errgo.Mask(err)
	}
	env.MaxBytesPerCharacter = uint(sb4)
	env.maxStringBytes = MaxStringChars * env.MaxBytesPerCharacter
	// Log.Debug("Env", "maxBytesPerCharacter", env.maxBytesPerCharacter)

	// acquire whether character set is fixed width
	if err = env.CheckStatus(C.OCINlsNumericInfoGet(unsafe.Pointer(env.handle),
		env.errorHandle, &sb4, C.OCI_NLS_CHARSET_FIXEDWIDTH),
		"Environment_New(): determine if charset fixed width",
	); err != nil {
		return nil, errgo.Mask(err)
	}
	env.FixedWidth = sb4 > 0

	var e error
	// determine encodings to use for Unicode values
	if env.Encoding, e = env.GetCharacterSetName(C.OCI_ATTR_ENV_CHARSET_ID); e != nil {
		return nil, e
	}
	if env.Nencoding, e = env.GetCharacterSetName(C.OCI_ATTR_ENV_NCHARSET_ID); e != nil {
		return nil, e
	}

	return env, nil
}

// Free frees the used handles
func (env *Environment) Free() error {
	if env.errorHandle != nil {
		C.OCIHandleFree(unsafe.Pointer(env.errorHandle), C.OCI_HTYPE_ERROR)
		env.errorHandle = nil
	}
	//if !env.cloneEnv {
	if env.handle != nil {
		C.OCIHandleFree(unsafe.Pointer(env.handle), C.OCI_HTYPE_ENV)
		env.handle = nil
	}
	env.numberToStringFormatBuffer = nil
	env.numberFromStringFormatBuffer = nil
	env.nlsNumericCharactersBuffer = nil
	//}
	return nil
}

func ociHandleAlloc(parent unsafe.Pointer, typ C.ub4, dst *unsafe.Pointer, at string) error {
	// var vsize C.ub4
	if err := checkStatus(C.OCIHandleAlloc(parent, dst, typ, C.size_t(0), nil), false); err != nil {
		return errgo.New(at + ": " + err.Error())
	}
	return nil
}

func (env *Environment) ociDescrAlloc(dst *unsafe.Pointer, typ C.ub4, at string) error {
	if err := checkStatus(
		C.OCIDescriptorAlloc(unsafe.Pointer(env.handle), dst, typ, C.size_t(0), nil),
		false); err != nil {
		return errgo.New(at + ": " + err.Error())
	}
	return nil
}

//AttrSet sets an attribute on the given parent pointer
func (env *Environment) AttrSet(parent unsafe.Pointer, parentTyp C.ub4,
	key C.ub4, value unsafe.Pointer, valueLength int) error {
	return env.CheckStatus(C.OCIAttrSet(parent, parentTyp,
		value, C.ub4(valueLength),
		key, env.errorHandle),
		"AttrSet")
}

// GetCharacterSetName retrieves the IANA character set name for the attribute.
func (env *Environment) GetCharacterSetName(attribute uint32) (string, error) {
	//, overrideValue string
	var (
		charsetID, vsize C.ub4
		status           C.sword
		err              error
	)

	/*
	   // if override value specified, use it
	   if (overrideValue) {
	       *result = PyMem_Malloc(strlen(overrideValue) + 1);
	       if (!*result)
	           return -1;
	       strcpy(*result, overrideValue);
	       return 0;
	   }
	*/

	// get character set id
	status = C.OCIAttrGet(unsafe.Pointer(env.handle), //void *trgthndlp
		C.OCI_HTYPE_ENV,            //ub4 trghndltyp
		unsafe.Pointer(&charsetID), //void *attributep
		&vsize,           //ub4 *sizep
		C.ub4(attribute), //ub4 attrtype
		env.errorHandle)  //OCIError *errhp
	if err = env.CheckStatus(status, "GetCharacterSetName[get charset id]"); err != nil {
		return "", errgo.Mask(err)
	}

	charsetName := make([]byte, C.OCI_NLS_MAXBUFSZ)
	ianaCharsetName := make([]byte, C.OCI_NLS_MAXBUFSZ)

	// get character set name
	if err = env.CheckStatus(C.OCINlsCharSetIdToName(unsafe.Pointer(env.handle),
		(*C.oratext)(&charsetName[0]),
		C.OCI_NLS_MAXBUFSZ, C.ub2(charsetID)),
		"GetCharacterSetName[get Oracle charset name]"); err != nil {
		return "", errgo.Mask(

			// get IANA character set name
			err)
	}

	status = C.OCINlsNameMap(unsafe.Pointer(env.handle),
		(*C.oratext)(&ianaCharsetName[0]),
		C.OCI_NLS_MAXBUFSZ, (*C.oratext)(&charsetName[0]),
		C.OCI_NLS_CS_ORA_TO_IANA)
	if err = env.CheckStatus(status, "GetCharacterSetName[translate NLS charset]"); err != nil {
		return "", errgo.Mask(

			// store results
			// oratext = unsigned char
			err)
	}

	return string(ianaCharsetName), nil
}

// Error handling
var (
	Warning        = &Error{Code: -(1<<30 - 1), Message: "WARNING"}
	NeedData       = &Error{Code: -(1<<30 - 2), Message: "NEED_DATA"}
	NoDataFound    = &Error{Code: -(1<<30 - 3), Message: "NO_DATA_FOUND"}
	StillExecuting = &Error{Code: -(1<<30 - 4), Message: "STILL_EXECUTING"}
	Continue       = &Error{Code: -(1<<30 - 5), Message: "CONTINUE"}
	Other          = &Error{Code: -(1<<30 - 6), Message: "other"}
	InvalidHandle  = &Error{Code: -(1<<30 - 7), Message: "invalid handle"}
)

// Check for an error in the last call and if an error has occurred.
func checkStatus(status C.sword, justSpecific bool) error {
	switch status {
	case C.OCI_SUCCESS, C.OCI_SUCCESS_WITH_INFO:
		return nil
	case C.OCI_NEED_DATA:
		return NeedData
	case C.OCI_NO_DATA:
		return NoDataFound
	case C.OCI_STILL_EXECUTING:
		return StillExecuting
	case C.OCI_CONTINUE:
		return Continue
	case C.OCI_INVALID_HANDLE:
		return InvalidHandle
	}
	if !justSpecific {
		return Other
	}
	return nil
}

// CheckStatus checks for an error in the last call and if an error has occurred,
// retrieve the error message from the database environment
func (env *Environment) CheckStatus(status C.sword, at string) error {
	// if status != C.OCI_SUCCESS {
	// Log.Debug("CheckStatus", "status", status, "success?", status == C.OCI_SUCCESS, "at", at)
	// }
	if status == C.OCI_SUCCESS || status == C.OCI_SUCCESS_WITH_INFO {
		// Log.Debug("CheckStatus: OK", "status", status)
		return nil
	}
	if err := checkStatus(status, true); err != nil {
		if err != NoDataFound {
			Log.Error("CheckStatus", "status", status, "at", at, "error", err, "stackTrace", getStackTrace())
		}
		return err
	}
	var (
		errorcode int
		ec        C.sb4
		errbuf    = make([]byte, 2001)
		i         = C.ub4(0)
		errstat   C.sword
		message   = make([]byte, 0, 2000)
	)
	for {
		i++
		errstat = C.OCIErrorGet(unsafe.Pointer(env.errorHandle), i, nil,
			&ec, (*C.OraText)(&errbuf[0]), C.ub4(len(errbuf)-1),
			C.OCI_HTYPE_ERROR)
		if ec != 0 && errorcode == 0 {
			errorcode = int(ec)
		}
		message = append(message, errbuf[:bytes.IndexByte(errbuf, 0)]...)
		if errstat == C.OCI_NO_DATA {
			break
		}
	}
	return NewErrorAt(errorcode, fmt.Sprintf("[%d] %s", status, message), at)
}

//AttrGet gets the parent's attribute identified by key, and stores it in dst
func (env *Environment) AttrGet(parent unsafe.Pointer, parentType, key int,
	dst unsafe.Pointer, errText string) (int, error) {
	var osize C.ub4
	if CTrace {
		ctrace("OCIAttrGet(parent=%p, parentType=%d, dst=%p, osize=%p, key=%d, env=%p)",
			parent, C.ub4(parentType), dst, &osize, C.ub4(key), env.errorHandle)
	}
	if err := env.CheckStatus(
		C.OCIAttrGet(parent, C.ub4(parentType), dst, &osize, C.ub4(key),
			env.errorHandle), errText); err != nil {
		Log.Error("AttrGet", "attr", key, "error", err)
		return -1, err
	}
	return int(osize), nil
}

//AttrGetName retrieves the name of the parent's attribute identified by key
func (env *Environment) AttrGetName(parent unsafe.Pointer, parentType, key int,
	errText string) ([]byte, error) {
	var (
		nameLen C.ub4
		status  C.sword
	)
	if CTrace {
		ctrace("OCIAttrGetName", parent, C.ub4(parentType),
			C.ub4(key), env.errorHandle,
			&status, &nameLen)
	}
	name := C.AttrGetName(parent, C.ub4(parentType),
		C.ub4(key), env.errorHandle,
		&status, &nameLen)
	if err := env.CheckStatus(status, errText); err != nil {
		Log.Error("AttrGetName", "attr", key, "error", err)
		return nil, err
	}
	result := C.GoBytes(unsafe.Pointer(name), C.int(nameLen))
	return result, nil
}

//FromEncodedString returns string decoded from Oracle's representation
func (env *Environment) FromEncodedString(text []byte) string {
	return string(text)
}
