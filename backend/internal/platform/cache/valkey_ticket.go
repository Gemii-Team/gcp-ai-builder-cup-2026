package cache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"kiwi/internal/platform/errs"
)

func ticketKey(ticket string) string { return "rtt:" + ticket }

// IssueTicket stores a one-time realtime ticket for t and returns it. It
// expires after Options.TicketTTL.
func (c *Valkey) IssueTicket(ctx context.Context, t TicketRecord) (string, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("cache: encode ticket: %w", err)
	}
	ticket := rand.Text() // 128 random bits
	if err := c.rdb.Set(ctx, ticketKey(ticket), raw, c.opt.TicketTTL).Err(); err != nil {
		return "", fmt.Errorf("cache: issue ticket: %w", err)
	}
	return ticket, nil
}

// RedeemTicket returns the record a ticket was issued for and deletes it in one
// step (GETDEL): of any number of concurrent redeems exactly one succeeds, the
// rest and every later one get ErrNotFound.
func (c *Valkey) RedeemTicket(ctx context.Context, ticket string) (TicketRecord, error) {
	raw, err := c.rdb.GetDel(ctx, ticketKey(ticket)).Bytes()
	if errors.Is(err, redis.Nil) {
		return TicketRecord{}, fmt.Errorf("cache: ticket: %w", errs.ErrNotFound)
	}
	if err != nil {
		return TicketRecord{}, fmt.Errorf("cache: redeem ticket: %w", err)
	}
	var t TicketRecord
	if err := json.Unmarshal(raw, &t); err != nil {
		return TicketRecord{}, fmt.Errorf("cache: decode ticket: %w", err)
	}
	return t, nil
}
