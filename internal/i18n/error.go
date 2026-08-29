package i18n

// NewError returns an error that looks its message up when it is printed rather
// than when it is built. That is what a package-level sentinel needs: package
// initialization runs before the language is resolved, so a sentinel built as
// errors.New(T(key)) freezes at whatever the environment suggested and the
// user's saved preference never reaches it.
//
// The returned value is the identity callers match on with errors.Is. It
// implements no Is method on purpose: two sentinels that happen to share a key
// are two distinct errors, and comparing keys would quietly merge them.
func NewError(key string) error {
	return &catalogError{key: key}
}

type catalogError struct {
	key string
}

func (e *catalogError) Error() string {
	return T(e.key)
}
