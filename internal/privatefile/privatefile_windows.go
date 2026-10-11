//go:build windows

package privatefile

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ownerAndDACL = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

func ownerSecurity(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, ErrPrivate
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, ErrPrivate
	}
	sid := user.User.Sid.String()
	if sid == "" {
		return nil, ErrPrivate
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;" + inherit + ";FA;;;" + sid + ")")
	if err != nil {
		return nil, ErrPrivate
	}
	return sd, nil
}

func makeDirectory(path string) error {
	sd, err := ownerSecurity(true)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ErrPrivate
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, &attributes); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

func privateDirectory(path string, _ os.FileInfo) bool {
	expected, err := ownerSecurity(true)
	if err != nil {
		return false
	}
	actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, ownerAndDACL)
	return err == nil && actual != nil && expected.String() != "" && actual.String() == expected.String()
}

func privateFile(f *os.File, _ os.FileInfo) bool {
	expected, err := ownerSecurity(false)
	if err != nil {
		return false
	}
	actual, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, ownerAndDACL)
	return err == nil && actual != nil && expected.String() != "" && actual.String() == expected.String()
}

func privateTemp(dir string) (*os.File, error) {
	sd, err := ownerSecurity(false)
	if err != nil {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, ".identity-"+hex.EncodeToString(random[:]))
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, ErrPrivate
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, ErrPrivate
	}
	return os.NewFile(uintptr(handle), name), nil
}
