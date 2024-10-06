// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ottlfuncs

import (
	"context"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/stretchr/testify/assert"
)

func Test_stringUnquoteFunc(t *testing.T) {
	tests := []struct {
		name     string
		target   ottl.StringGetter[any]
		expected string
	}{
		{
			name: "unquote empty string",
			target: &ottl.StandardStringGetter[any]{
				Getter: func(_ context.Context, _ any) (any, error) {
					return "", nil
				},
			},
			expected: "",
		},
		{
			name: "unquote escaped json string",
			target: &ottl.StandardStringGetter[any]{
				Getter: func(_ context.Context, _ any) (any, error) {
					return `{\"host\":\"154.89.54.124\",\"user-identifier\":\"mckenzie1244\",\"datetime\":\"05/Oct/2024:16:25:26 +0000\",\"method\":\"HEAD\",\"request\":\"/leading-edge/systems\",\"protocol\":\"HTTP/1.1\",\"status\":201,\"bytes\":5520,\"referer\":\"https://www.nationalportals.com/grow/transform\"}`, nil
				},
			},
			expected: `{"host":"154.89.54.124","user-identifier":"mckenzie1244","datetime":"05/Oct/2024:16:25:26 +0000","method":"HEAD","request":"/leading-edge/systems","protocol":"HTTP/1.1","status":201,"bytes":5520,"referer":"https://www.nationalportals.com/grow/transform"}`,
		},
		{
			name: "json string no-op unquote",
			target: &ottl.StandardStringGetter[any]{
				Getter: func(_ context.Context, _ any) (any, error) {
					return `{"host":"154.89.54.124","user-identifier":"mckenzie1244","datetime":"05/Oct/2024:16:25:26 +0000","method":"HEAD","request":"/leading-edge/systems","protocol":"HTTP/1.1","status":304,"bytes":5520,"referer":"https://www.nationalportals.com/grow/transform"}`, nil
				},
			},
			expected: `{"host":"154.89.54.124","user-identifier":"mckenzie1244","datetime":"05/Oct/2024:16:25:26 +0000","method":"HEAD","request":"/leading-edge/systems","protocol":"HTTP/1.1","status":304,"bytes":5520,"referer":"https://www.nationalportals.com/grow/transform"}`,
		},
	}

	for _, tests := range tests {
		t.Run(tests.name, func(t *testing.T) {
			exprFunc := stringReplaceAllFunc[any](tests.target)
			out, err := exprFunc(context.Background(), nil)
			assert.NoError(t, err)
			assert.Equal(t, tests.expected, out)
		})
	}
}

func Benchmark_stringUnquoteFunc(b *testing.B) {
	target := &ottl.StandardStringGetter[any]{
		Getter: func(_ context.Context, _ any) (any, error) {
			return `{\"host\":\"154.89.54.124\",\"user-identifier\":\"mckenzie1244\",\"datetime\":\"05/Oct/2024:16:25:26 +0000\",\"method\":\"HEAD\",\"request\":\"/leading-edge/systems\",\"protocol\":\"HTTP/1.1\",\"status\":201,\"bytes\":5520,\"referer\":\"https://www.nationalportals.com/grow/transform\"}`, nil
		},
	}
	exprFunc := stringReplaceAllFunc[any](target)
	ctx := context.Background()
	tCtx := context.Background()
	for i := 0; i < b.N; i++ {
		v, err := exprFunc(ctx, tCtx)
		if err != nil {
			b.Error(err)
		}
		if v != `{"host":"154.89.54.124","user-identifier":"mckenzie1244","datetime":"05/Oct/2024:16:25:26 +0000","method":"HEAD","request":"/leading-edge/systems","protocol":"HTTP/1.1","status":201,"bytes":5520,"referer":"https://www.nationalportals.com/grow/transform"}` {
			b.Fail()
		}
	}
}
