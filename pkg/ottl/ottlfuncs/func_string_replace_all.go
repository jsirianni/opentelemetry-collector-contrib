// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ottlfuncs // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
)

type StringReplaceAllArguments[K any] struct {
	Target ottl.StringGetter[K]
}

func NewStringReplaceAllFactory[K any]() ottl.Factory[K] {
	return ottl.NewFactory("string_replace_all", &StringReplaceAllArguments[K]{}, createStringReplaceAllFunction[K])
}

func createStringReplaceAllFunction[K any](_ ottl.FunctionContext, oArgs ottl.Arguments) (ottl.ExprFunc[K], error) {
	args, ok := oArgs.(*StringReplaceAllArguments[K])

	if !ok {
		return nil, fmt.Errorf("StringUnquoteFactory args must be of type *StringUnquoteArguments[K]")
	}

	return stringReplaceAllFunc(args.Target), nil
}

func stringReplaceAllFunc[K any](target ottl.StringGetter[K]) ottl.ExprFunc[K] {
	return func(ctx context.Context, tCtx K) (any, error) {
		value, err := target.Get(ctx, tCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to get value: %w", err)
		}
		return strings.ReplaceAll(value, `\"`, `"`), nil
	}
}
