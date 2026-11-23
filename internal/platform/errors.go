package platform

import (
	"errors"
	"fmt"
)

// UserError 显式标记面向用户的业务校验错误，避免把底层 DB/系统报错原文暴露给页面。
type UserError struct {
	Msg string
}

func (e UserError) Error() string { return e.Msg }

func UserErr(msg string) error {
	return UserError{Msg: msg}
}

func UserErrf(format string, args ...any) error {
	return UserError{Msg: fmt.Sprintf(format, args...)}
}

// UserErrorMessage 仅对显式标记的业务校验错误返回可展示文案。
func UserErrorMessage(err error) (string, bool) {
	var ue UserError
	if errors.As(err, &ue) {
		return err.Error(), true
	}
	return "", false
}
