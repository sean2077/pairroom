package enterpriseclient

import (
	"context"
	"net/http"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
)

func (b *BindingClient) history(ctx context.Context, r bindingRecord, query relay.HistoryQuery) (relay.HistoryPage, error) {
	if !r.Grant.ExpiresAt.After(time.Now()) {
		return relay.HistoryPage{}, ErrBindingCredentialExpired
	}
	status, data, err := b.parent.request(ctx, http.MethodPost, devicePath+"/rooms/"+r.RoomID+"/relay/history", query, r.Grant.Token, 2<<20)
	if err != nil {
		return relay.HistoryPage{}, err
	}
	if status != http.StatusOK {
		return relay.HistoryPage{}, wireFailure(status, data)
	}
	var page relay.HistoryPage
	if decodeWire(data, &page) != nil || page.Total < 0 || len(page.Messages) > relay.HistoryPageLimit || len(page.Delivery) > relay.HistoryPageLimit || page.HasMore != (page.NextCursor != "") {
		return relay.HistoryPage{}, unknown("invalid_response")
	}
	if query.ID != "" && (len(page.Messages) > 1 || len(page.Messages) == 1 && page.Messages[0].ID != query.ID) {
		return relay.HistoryPage{}, unknown("invalid_response")
	}
	for _, message := range page.Messages {
		if !validID(message.ID) || message.Receipt != "" {
			return relay.HistoryPage{}, unknown("invalid_response")
		}
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return relay.HistoryPage{}, err
	}
	return page, nil
}
