package enterpriseservice

import (
	"context"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

func (b *fakeBackend) AuthorizeRoomRead(_ context.Context, auth sqlstore.AuthContext, roomID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAuth, b.lastRoomID = auth, roomID
	return b.readErr
}
