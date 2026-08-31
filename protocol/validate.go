package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

func (e Envelope) Validate() error {
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		return errors.New("protocol: envelope has invalid payload")
	}
	switch e.Kind {
	case KindHello:
		if e.RequestID != 0 || e.Sequence != 0 {
			return errors.New("protocol: hello has correlation fields")
		}
	case KindSnapshot:
		if e.RequestID != 0 {
			return errors.New("protocol: snapshot has a request ID")
		}
	case KindItemAdded, KindItemChanged, KindItemRemoved, KindMenuUpdated:
		if e.Sequence == 0 || e.RequestID != 0 {
			return errors.New("protocol: state message has invalid sequence")
		}
	case KindCommand, KindReply:
		if e.RequestID == 0 || e.Sequence != 0 {
			return errors.New("protocol: request message has invalid request ID")
		}
	default:
		return fmt.Errorf("protocol: unknown message kind %q", e.Kind)
	}
	return nil
}

func ValidateNextSequence(previous, next uint64) error {
	if previous == ^uint64(0) || next != previous+1 {
		return fmt.Errorf("protocol: sequence %d does not follow %d", next, previous)
	}
	return nil
}

func (h Hello) Validate(role string) error {
	if h.Major != ProtocolMajor {
		return fmt.Errorf("protocol: incompatible major %d", h.Major)
	}
	if h.Role != role {
		return fmt.Errorf("protocol: unexpected role %q", h.Role)
	}
	seen := make(map[string]struct{}, len(h.Capabilities))
	for _, capability := range h.Capabilities {
		if err := validateText("capability", capability, MaxTextBytes, false); err != nil {
			return err
		}
		if _, exists := seen[capability]; exists {
			return fmt.Errorf("protocol: duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func (k ItemKey) Validate() error {
	if err := validateText("owner", k.Owner, MaxTextBytes, false); err != nil {
		return err
	}
	if !strings.HasPrefix(k.Owner, ":") || len(k.Owner) < 2 {
		return fmt.Errorf("protocol: owner %q is not a unique bus name", k.Owner)
	}
	if err := validateText("object path", k.ObjectPath, MaxTextBytes, false); err != nil {
		return err
	}
	if !strings.HasPrefix(k.ObjectPath, "/") {
		return fmt.Errorf("protocol: object path %q is not absolute", k.ObjectPath)
	}
	if k.Generation == 0 {
		return errors.New("protocol: item generation is zero")
	}
	return nil
}

func (s Snapshot) Validate() error {
	if len(s.Items) > MaxItems {
		return errors.New("protocol: snapshot exceeds item limit")
	}
	seen := make(map[ItemKey]struct{}, len(s.Items))
	var aggregate uint64
	for i := range s.Items {
		if err := s.Items[i].Validate(); err != nil {
			return fmt.Errorf("protocol: items[%d]: %w", i, err)
		}
		if _, exists := seen[s.Items[i].Key]; exists {
			return fmt.Errorf("protocol: duplicate item %v", s.Items[i].Key)
		}
		seen[s.Items[i].Key] = struct{}{}
		bytes, err := s.Items[i].pixmapBytes()
		if err != nil {
			return err
		}
		aggregate += bytes
		if aggregate > MaxPixmapBytesAggregate {
			return errors.New("protocol: snapshot exceeds aggregate pixmap budget")
		}
	}
	return nil
}

func (i Item) Validate() error {
	if err := i.Key.Validate(); err != nil {
		return err
	}
	if err := validateText("item ID", i.ID, MaxTextBytes, false); err != nil {
		return err
	}
	if err := validateText("title", i.Title, MaxTextBytes, true); err != nil {
		return err
	}
	if !i.Category.valid() {
		return fmt.Errorf("protocol: invalid category %q", i.Category)
	}
	if !i.Status.valid() {
		return fmt.Errorf("protocol: invalid status %q", i.Status)
	}
	for name, icon := range map[string]Icon{
		"icon": i.Icon, "attention icon": i.AttentionIcon, "overlay icon": i.OverlayIcon,
	} {
		if err := icon.Validate(name); err != nil {
			return err
		}
	}
	if err := i.Tooltip.Validate(); err != nil {
		return err
	}
	if i.MenuPath != "" {
		if err := validateText("menu path", i.MenuPath, MaxTextBytes, false); err != nil {
			return err
		}
		if !strings.HasPrefix(i.MenuPath, "/") {
			return fmt.Errorf("protocol: menu path %q is not absolute", i.MenuPath)
		}
	}
	total, err := i.pixmapBytes()
	if err != nil {
		return err
	}
	if total > MaxPixmapBytesPerItem {
		return errors.New("protocol: item exceeds pixmap budget")
	}
	return nil
}

func (i Item) pixmapBytes() (uint64, error) {
	var total uint64
	for _, set := range [][]Pixmap{i.Icon.Pixmaps, i.AttentionIcon.Pixmaps, i.OverlayIcon.Pixmaps, i.Tooltip.Pixmaps} {
		for _, pixmap := range set {
			if err := pixmap.Validate(); err != nil {
				return 0, err
			}
			total += uint64(len(pixmap.ARGB))
		}
	}
	return total, nil
}

func (i Icon) Validate(name string) error {
	if err := validateText(name+" name", i.Name, MaxTextBytes, true); err != nil {
		return err
	}
	if err := validateText(name+" theme path", i.ThemePath, MaxTextBytes, true); err != nil {
		return err
	}
	for j := range i.Pixmaps {
		if err := i.Pixmaps[j].Validate(); err != nil {
			return fmt.Errorf("protocol: %s pixmap[%d]: %w", name, j, err)
		}
	}
	return nil
}

func (p Pixmap) Validate() error {
	if p.Width == 0 || p.Height == 0 || p.Width > MaxPixmapDimension || p.Height > MaxPixmapDimension {
		return fmt.Errorf("protocol: invalid pixmap dimensions %dx%d", p.Width, p.Height)
	}
	if uint64(len(p.ARGB)) != uint64(p.Width)*uint64(p.Height)*4 {
		return errors.New("protocol: pixmap data does not match its dimensions")
	}
	if len(p.ARGB) > MaxPixmapBytesPerItem {
		return errors.New("protocol: pixmap exceeds the per-item budget")
	}
	return nil
}

func (t Tooltip) Validate() error {
	if err := validateText("tooltip icon name", t.IconName, MaxTextBytes, true); err != nil {
		return err
	}
	if err := validateText("tooltip title", t.Title, MaxTooltipBytes, true); err != nil {
		return err
	}
	if err := validateText("tooltip description", t.Description, MaxTooltipBytes, true); err != nil {
		return err
	}
	for i := range t.Pixmaps {
		if err := t.Pixmaps[i].Validate(); err != nil {
			return fmt.Errorf("protocol: tooltip pixmap[%d]: %w", i, err)
		}
	}
	return nil
}

func (m Menu) Validate() error {
	if m.Root.ID != 0 {
		return errors.New("protocol: menu root is not ID 0")
	}
	seen := make(map[int32]struct{}, 16)
	var nodes int
	var icons uint64
	return m.Root.validate(0, seen, &nodes, &icons)
}

func (n MenuNode) validate(depth int, seen map[int32]struct{}, nodes *int, icons *uint64) error {
	if depth > MaxMenuDepth {
		return fmt.Errorf("protocol: menu depth exceeds %d", MaxMenuDepth)
	}
	*nodes++
	if *nodes > MaxMenuNodes {
		return fmt.Errorf("protocol: menu exceeds %d nodes", MaxMenuNodes)
	}
	if n.ID < 0 {
		return fmt.Errorf("protocol: negative menu ID %d", n.ID)
	}
	if _, exists := seen[n.ID]; exists {
		return fmt.Errorf("protocol: duplicate menu ID %d", n.ID)
	}
	seen[n.ID] = struct{}{}
	if err := validateText("menu label", n.Label, MaxTextBytes, true); err != nil {
		return err
	}
	if err := validateText("menu icon name", n.IconName, MaxTextBytes, true); err != nil {
		return err
	}
	if !n.ToggleType.valid() {
		return fmt.Errorf("protocol: invalid toggle type %q", n.ToggleType)
	}
	switch n.ChildrenDisplay {
	case "", "submenu":
	default:
		return fmt.Errorf("protocol: invalid children display %q", n.ChildrenDisplay)
	}
	*icons += uint64(len(n.IconData))
	if *icons > MaxMenuIconBytes {
		return errors.New("protocol: menu exceeds its icon budget")
	}
	for i := range n.Children {
		if err := n.Children[i].validate(depth+1, seen, nodes, icons); err != nil {
			return err
		}
	}
	return nil
}

func (c Command) Validate() error {
	if err := c.Item.Validate(); err != nil {
		return err
	}
	if err := validateCoordinate(c.X); err != nil {
		return err
	}
	if err := validateCoordinate(c.Y); err != nil {
		return err
	}
	if c.MenuID < 0 {
		return fmt.Errorf("protocol: negative menu ID %d", c.MenuID)
	}
	if len(c.Params) > 0 && !json.Valid(c.Params) {
		return errors.New("protocol: command params are not valid JSON")
	}
	switch c.Kind {
	case CommandActivate, CommandSecondaryActivate, CommandMenuOpen, CommandMenuClose:
	case CommandScroll:
		if c.Delta == 0 {
			return errors.New("protocol: scroll delta is zero")
		}
		if !c.Orientation.valid() {
			return fmt.Errorf("protocol: invalid scroll orientation %q", c.Orientation)
		}
	case CommandMenuSelect:
		if c.MenuID == 0 {
			return errors.New("protocol: menu selection names the root")
		}
	case CommandAboutToShow:
	default:
		return fmt.Errorf("protocol: invalid command kind %q", c.Kind)
	}
	return nil
}

func (r Reply) Validate() error {
	if r.OK == (r.Error != nil) {
		return errors.New("protocol: reply must contain one outcome")
	}
	if !r.Item.IsZero() {
		if err := r.Item.Validate(); err != nil {
			return err
		}
	}
	if r.Error == nil {
		return nil
	}
	if !r.Error.Code.valid() {
		return fmt.Errorf("protocol: invalid error code %q", r.Error.Code)
	}
	return validateText("error message", r.Error.Message, MaxTextBytes, true)
}

func validateCoordinate(value int32) error {
	if value > MaxCoordinate || value < -MaxCoordinate {
		return fmt.Errorf("protocol: coordinate %d is out of range", value)
	}
	return nil
}

func validateText(name, value string, limit int, emptyOK bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("protocol: %s is not valid UTF-8", name)
	}
	if (!emptyOK && value == "") || len(value) > limit {
		return fmt.Errorf("protocol: invalid %s length", name)
	}
	return nil
}

func (c Category) valid() bool {
	switch c {
	case CategoryApplicationStatus, CategoryCommunications, CategorySystemServices, CategoryHardware:
		return true
	default:
		return false
	}
}

func (s Status) valid() bool {
	switch s {
	case StatusPassive, StatusActive, StatusNeedsAttention:
		return true
	default:
		return false
	}
}

func (t ToggleType) valid() bool {
	switch t {
	case ToggleNone, ToggleCheckmark, ToggleRadio:
		return true
	default:
		return false
	}
}

func (o ScrollOrientation) valid() bool {
	switch o {
	case ScrollVertical, ScrollHorizontal:
		return true
	default:
		return false
	}
}

func (c ErrorCode) valid() bool {
	switch c {
	case ErrorInvalid, ErrorStaleItem, ErrorStaleRevision, ErrorUnavailable, ErrorBusy:
		return true
	default:
		return false
	}
}

// Error lets a typed protocol failure travel as a Go error inside the service
// before it is serialized into a reply.
func (e *ProtocolError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}
