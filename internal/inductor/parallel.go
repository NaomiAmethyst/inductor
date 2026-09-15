// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/json"
	"sync"
)

func jsonBytes(v any) ([]byte, error)  { return json.Marshal(v) }
func decodeJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

// parallelMap bounds concurrency and keeps each error associated with its input.
func parallelMap[T, R any](ctx context.Context, inputs []T, workers int, fn func(context.Context, T) (R, error)) ([]R, []error) {
	out := make([]R, len(inputs))
	errs := make([]error, len(inputs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < min(max(1, workers), len(inputs)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					errs[i] = ctx.Err()
					continue
				}
				out[i], errs[i] = fn(ctx, inputs[i])
			}
		}()
	}
	for i := range inputs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, errs
}
