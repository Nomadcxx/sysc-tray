package protocol

import "encoding/json"

const (
	ProtocolMajor uint16 = 1
	ProtocolMinor uint16 = 0

	RolePresenter  = "presenter"
	CapabilityTray = "tray-state"

	MaxItems                  = 128
	MaxTextBytes              = 4 << 10
	MaxTooltipBytes           = 2 << 10
	MaxPixmapDimension        = 512
	MaxPixmapBytesPerItem     = 2 << 20
	MaxPixmapBytesAggregate   = 8 << 20
	MaxMenuDepth              = 8
	MaxMenuNodes              = 512
	MaxMenuIconBytes          = 2 << 20
	MaxCoordinate             = 1 << 16
	MaxPresenterQueueMessages = 256
	MaxPresenterDecodedBytes  = 32 << 20
)

const (
	KindHello       = "hello"
	KindSnapshot    = "snapshot"
	KindItemAdded   = "item-added"
	KindItemChanged = "item-changed"
	KindItemRemoved = "item-removed"
	KindMenuUpdated = "menu-updated"
	KindCommand     = "command"
	KindReply       = "reply"
)

type Envelope struct {
	Kind      string          `json:"kind"`
	RequestID uint64          `json:"request_id,omitempty"`
	Sequence  uint64          `json:"sequence,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type Hello struct {
	Major        uint16   `json:"major"`
	Minor        uint16   `json:"minor"`
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
}

// ItemKey names one StatusNotifierItem generation. The owner is the unique bus
// name; a re-registered owner and path pair takes a fresh generation so stale
// commands cannot reach the replacement.
type ItemKey struct {
	Owner      string `json:"owner"`
	ObjectPath string `json:"object_path"`
	Generation uint64 `json:"generation"`
}

func (k ItemKey) IsZero() bool {
	return k.Owner == "" && k.ObjectPath == "" && k.Generation == 0
}

type Category string

const (
	CategoryApplicationStatus Category = "ApplicationStatus"
	CategoryCommunications    Category = "Communications"
	CategorySystemServices    Category = "SystemServices"
	CategoryHardware          Category = "Hardware"
)

type Status string

const (
	StatusPassive        Status = "Passive"
	StatusActive         Status = "Active"
	StatusNeedsAttention Status = "NeedsAttention"
)

type Pixmap struct {
	Width  uint32 `json:"width"`
	Height uint32 `json:"height"`
	ARGB   []byte `json:"argb"`
}

type Icon struct {
	Name      string   `json:"name,omitempty"`
	ThemePath string   `json:"theme_path,omitempty"`
	Pixmaps   []Pixmap `json:"pixmaps,omitempty"`
}

type Tooltip struct {
	IconName    string   `json:"icon_name,omitempty"`
	Pixmaps     []Pixmap `json:"pixmaps,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
}

type Item struct {
	Key           ItemKey  `json:"key"`
	ID            string   `json:"id"`
	Title         string   `json:"title,omitempty"`
	Category      Category `json:"category"`
	Status        Status   `json:"status"`
	Icon          Icon     `json:"icon"`
	AttentionIcon Icon     `json:"attention_icon,omitempty"`
	OverlayIcon   Icon     `json:"overlay_icon,omitempty"`
	Tooltip       Tooltip  `json:"tooltip,omitempty"`
	MenuPath      string   `json:"menu_path,omitempty"`
	ItemIsMenu    bool     `json:"item_is_menu,omitempty"`
	// CloseSupported is set only when the service established a same-UID
	// process identity for the item's owner and can terminate it gracefully.
	CloseSupported bool `json:"close_supported,omitempty"`
}

type Snapshot struct {
	Sequence uint64 `json:"sequence"`
	Items    []Item `json:"items"`
}

type ItemRemoved struct {
	Key ItemKey `json:"key"`
}

type MenuUpdate struct {
	Key  ItemKey `json:"key"`
	Menu Menu    `json:"menu"`
}

type ToggleType string

const (
	ToggleNone      ToggleType = ""
	ToggleCheckmark ToggleType = "checkmark"
	ToggleRadio     ToggleType = "radio"
)

type MenuNode struct {
	ID              int32      `json:"id"`
	Label           string     `json:"label,omitempty"`
	Enabled         bool       `json:"enabled,omitempty"`
	Visible         bool       `json:"visible,omitempty"`
	Separator       bool       `json:"separator,omitempty"`
	IconName        string     `json:"icon_name,omitempty"`
	IconData        []byte     `json:"icon_data,omitempty"`
	ToggleType      ToggleType `json:"toggle_type,omitempty"`
	ToggleState     int32      `json:"toggle_state,omitempty"`
	ChildrenDisplay string     `json:"children_display,omitempty"`
	Children        []MenuNode `json:"children,omitempty"`
}

// Menu carries one DBusMenu layout revision. Commands naming an older revision
// are refused rather than replayed against a changed tree.
type Menu struct {
	Revision uint32   `json:"revision"`
	Root     MenuNode `json:"root"`
}

type CommandKind string

const (
	CommandActivate          CommandKind = "activate"
	CommandSecondaryActivate CommandKind = "secondary-activate"
	CommandScroll            CommandKind = "scroll"
	CommandMenuOpen          CommandKind = "menu.open"
	CommandMenuSelect        CommandKind = "menu.select"
	CommandAboutToShow       CommandKind = "menu.about-to-show"
	CommandMenuClose         CommandKind = "menu.close"
	CommandTerminate         CommandKind = "terminate"
)

type ScrollOrientation string

const (
	ScrollVertical   ScrollOrientation = "vertical"
	ScrollHorizontal ScrollOrientation = "horizontal"
)

// Command is correlated by the envelope request ID; it carries no identifier of
// its own so one connection owns the whole correlation rule.
type Command struct {
	Kind         CommandKind       `json:"kind"`
	Item         ItemKey           `json:"item"`
	MenuRevision uint32            `json:"menu_revision,omitempty"`
	MenuID       int32             `json:"menu_id,omitempty"`
	Output       uint32            `json:"output,omitempty"`
	Serial       uint32            `json:"serial,omitempty"`
	X            int32             `json:"x,omitempty"`
	Y            int32             `json:"y,omitempty"`
	Delta        int32             `json:"delta,omitempty"`
	Orientation  ScrollOrientation `json:"orientation,omitempty"`
	Params       json.RawMessage   `json:"params,omitempty"`
}

type ErrorCode string

const (
	ErrorInvalid       ErrorCode = "invalid"
	ErrorStaleItem     ErrorCode = "stale_item"
	ErrorStaleRevision ErrorCode = "stale_revision"
	ErrorUnavailable   ErrorCode = "unavailable"
	ErrorBusy          ErrorCode = "busy"
)

type ProtocolError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

type Reply struct {
	OK     bool           `json:"ok"`
	Item   ItemKey        `json:"item,omitempty"`
	Output uint32         `json:"output,omitempty"`
	Error  *ProtocolError `json:"error,omitempty"`
}
