// Code generated by pegomock. DO NOT EDIT.
package matchers

import (
	"reflect"
	"github.com/petergtz/pegomock"
	cmd "github.com/jenkins-x/jx/pkg/jx/cmd"
)

func AnyCmdFactory() cmd.Factory {
	pegomock.RegisterMatcher(pegomock.NewAnyMatcher(reflect.TypeOf((*(cmd.Factory))(nil)).Elem()))
	var nullValue cmd.Factory
	return nullValue
}

func EqCmdFactory(value cmd.Factory) cmd.Factory {
	pegomock.RegisterMatcher(&pegomock.EqMatcher{Value: value})
	var nullValue cmd.Factory
	return nullValue
}
