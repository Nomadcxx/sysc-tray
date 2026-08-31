// Package item reads StatusNotifierItem properties over D-Bus and publishes
// bounded, renderer-neutral state. It never rasterizes, resolves a theme, or
// chooses a scale; those belong to the shell.
package item

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/dbusval"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

const ItemInterface = "org.kde.StatusNotifierItem"

// Refresh rates are bounded so a chatty application cannot starve the service.
const (
	PerItemInterval = time.Second / 30
	GlobalInterval  = time.Second / 120
)

// Limiter bounds refresh work per item and across the service. It is shared by
// every proxy, so one item's signal storm cannot crowd out its siblings.
type Limiter struct {
	now func() time.Time

	mu     sync.Mutex
	perKey map[protocol.ItemKey]time.Time
	last   time.Time
}

func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, perKey: make(map[protocol.ItemKey]time.Time)}
}

// Reserve claims the right to refresh now, or reports how long to wait. A
// successful reservation is recorded; a refused one is not.
func (l *Limiter) Reserve(key protocol.ItemKey) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	earliest := now
	if !l.last.IsZero() {
		if next := l.last.Add(GlobalInterval); next.After(earliest) {
			earliest = next
		}
	}
	if previous, ok := l.perKey[key]; ok {
		if next := previous.Add(PerItemInterval); next.After(earliest) {
			earliest = next
		}
	}
	if earliest.After(now) {
		return earliest.Sub(now)
	}
	l.last = now
	l.perKey[key] = now
	return 0
}

// Forget drops accounting for an item that has gone away.
func (l *Limiter) Forget(key protocol.ItemKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.perKey, key)
}

// normalize converts raw SNI properties into a bounded wire item. A malformed
// or missing field falls back to its specification default rather than failing
// the item, because one bad property must not remove a healthy tray entry.
func normalize(key protocol.ItemKey, props map[string]dbus.Variant) protocol.Item {
	item := protocol.Item{
		Key:        key,
		ID:         text(props, "Id", protocol.MaxTextBytes),
		Title:      text(props, "Title", protocol.MaxTextBytes),
		Category:   category(props),
		Status:     status(props),
		ItemIsMenu: boolean(props, "ItemIsMenu"),
		MenuPath:   objectPath(props, "Menu"),
	}
	if item.ID == "" {
		item.ID = key.Owner + key.ObjectPath
	}
	budget := protocol.MaxPixmapBytesPerItem
	item.Icon = icon(props, "IconName", "IconPixmap", "IconThemePath", &budget)
	item.AttentionIcon = icon(props, "AttentionIconName", "AttentionIconPixmap", "IconThemePath", &budget)
	item.OverlayIcon = icon(props, "OverlayIconName", "OverlayIconPixmap", "IconThemePath", &budget)
	item.Tooltip = tooltip(props, &budget)
	return item
}

func icon(props map[string]dbus.Variant, nameKey, pixmapKey, themeKey string, budget *int) protocol.Icon {
	result := protocol.Icon{
		Name:      text(props, nameKey, protocol.MaxTextBytes),
		ThemePath: text(props, themeKey, protocol.MaxTextBytes),
	}
	if variant, ok := props[pixmapKey]; ok {
		result.Pixmaps = decodePixmaps(variant, *budget)
		for _, pixmap := range result.Pixmaps {
			*budget -= len(pixmap.ARGB)
		}
	}
	return result
}

// tooltip reads the SNI `(sa(iiay)ss)` tooltip. Anything else leaves the
// tooltip empty rather than guessing at its parts.
func tooltip(props map[string]dbus.Variant, budget *int) protocol.Tooltip {
	variant, ok := props["ToolTip"]
	if !ok {
		return protocol.Tooltip{}
	}
	fields, ok := dbusval.Tuple(variant.Value(), 4)
	if !ok {
		return protocol.Tooltip{}
	}
	result := protocol.Tooltip{
		IconName:    clamp(dbusval.String(fields[0]), protocol.MaxTextBytes),
		Title:       clamp(dbusval.String(fields[2]), protocol.MaxTooltipBytes),
		Description: clamp(dbusval.String(fields[3]), protocol.MaxTooltipBytes),
	}
	if fields[1].IsValid() && fields[1].CanInterface() {
		result.Pixmaps = decodePixmaps(dbus.MakeVariant(fields[1].Interface()), *budget)
		for _, pixmap := range result.Pixmaps {
			*budget -= len(pixmap.ARGB)
		}
	}
	return result
}

func text(props map[string]dbus.Variant, name string, limit int) string {
	variant, ok := props[name]
	if !ok {
		return ""
	}
	value, ok := variant.Value().(string)
	if !ok {
		return ""
	}
	return clamp(value, limit)
}

func boolean(props map[string]dbus.Variant, name string) bool {
	variant, ok := props[name]
	if !ok {
		return false
	}
	value, _ := variant.Value().(bool)
	return value
}

func objectPath(props map[string]dbus.Variant, name string) string {
	variant, ok := props[name]
	if !ok {
		return ""
	}
	var candidate dbus.ObjectPath
	switch value := variant.Value().(type) {
	case dbus.ObjectPath:
		candidate = value
	case string:
		candidate = dbus.ObjectPath(value)
	default:
		return ""
	}
	if !candidate.IsValid() {
		return ""
	}
	return string(candidate)
}

func category(props map[string]dbus.Variant) protocol.Category {
	value := protocol.Category(text(props, "Category", protocol.MaxTextBytes))
	switch value {
	case protocol.CategoryApplicationStatus, protocol.CategoryCommunications,
		protocol.CategorySystemServices, protocol.CategoryHardware:
		return value
	default:
		return protocol.CategoryApplicationStatus
	}
}

func status(props map[string]dbus.Variant) protocol.Status {
	value := protocol.Status(text(props, "Status", protocol.MaxTextBytes))
	switch value {
	case protocol.StatusPassive, protocol.StatusActive, protocol.StatusNeedsAttention:
		return value
	default:
		return protocol.StatusActive
	}
}

// clamp keeps text valid UTF-8 and within its bound, cutting on a rune
// boundary so a truncated string never becomes invalid.
func clamp(value string, limit int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
