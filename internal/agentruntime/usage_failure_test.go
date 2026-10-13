package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

type partialUsageRequester struct{ response *model.ModelResponse }

func (r partialUsageRequester) CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error) {
	return r.response, errors.New("request interrupted")
}
func (r partialUsageRequester) StreamResponse(context.Context, model.ModelRequest, func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	return r.response, errors.New("stream interrupted")
}

func TestFailedRequestUsagePreservesActualUsageWithoutEstimates(t *testing.T) {
	for _, usage := range []model.ResponseUsage{{}, {InputTokens: 12}, {InputTokensDetails: model.TokenInputDetails{CachedTokens: 25}}} {
		for _, stream := range []bool{false, true} {
			tracked := NewUsageTrackingRequester(partialUsageRequester{response: &model.ModelResponse{Usage: usage}}, "lead", "profile", "provider", "model")
			observations := 0
			tracked.SetAgentUsageObserver(func(actual model.ResponseUsage, _, _, _, _ string, estimate int64) {
				observations++
				if actual != usage || estimate != 0 {
					t.Fatalf("actual=%+v estimate=%d", actual, estimate)
				}
			})
			request := model.ModelRequest{Input: model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("long enough to estimate input tokens")})}
			if stream {
				_, _ = tracked.StreamResponse(context.Background(), request, nil)
			} else {
				_, _ = tracked.CreateResponse(context.Background(), request)
			}
			want := 0
			if usage != (model.ResponseUsage{}) {
				want = 1
			}
			if observations != want {
				t.Fatalf("usage=%+v stream=%v observations=%d", usage, stream, observations)
			}
		}
	}
}
