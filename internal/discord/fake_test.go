package discord

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// fakeAPI is an in-memory Discord guild.
type fakeAPI struct {
	mu        sync.Mutex
	nextID    int
	channels  []*discordgo.Channel
	messages  map[string][]*discordgo.MessageSend // channelID → sent
	edits     []*discordgo.MessageEdit
	threads   []string
	pins      []string
	crossp    []string
	roles     []*discordgo.Role
	noNews    bool // simulate non-community guild
	failSends int  // fail the next N sends
}

func newFakeAPI() *fakeAPI { return &fakeAPI{messages: map[string][]*discordgo.MessageSend{}} }

func (f *fakeAPI) id() string {
	f.nextID++
	return fmt.Sprintf("%d", 1000+f.nextID)
}

func (f *fakeAPI) channel(id string) *discordgo.Channel {
	for _, c := range f.channels {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (f *fakeAPI) byName(name string) *discordgo.Channel {
	for _, c := range f.channels {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func notFound() error {
	return &discordgo.RESTError{Response: &http.Response{StatusCode: 404}}
}

func (f *fakeAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSends > 0 {
		f.failSends--
		return nil, fmt.Errorf("boom")
	}
	if f.channel(channelID) == nil {
		return nil, notFound()
	}
	f.messages[channelID] = append(f.messages[channelID], data)
	return &discordgo.Message{ID: f.id(), ChannelID: channelID}, nil
}

func (f *fakeAPI) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, m)
	return &discordgo.Message{ID: m.ID, ChannelID: m.Channel}, nil
}

func (f *fakeAPI) ChannelMessagePin(_, messageID string, _ ...discordgo.RequestOption) error {
	f.pins = append(f.pins, messageID)
	return nil
}

func (f *fakeAPI) ChannelMessageCrosspost(_, messageID string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.crossp = append(f.crossp, messageID)
	return &discordgo.Message{ID: messageID}, nil
}

func (f *fakeAPI) MessageThreadStart(_, _, name string, _ int, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.threads = append(f.threads, name)
	return &discordgo.Channel{ID: f.id(), Name: name}, nil
}

func (f *fakeAPI) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*discordgo.Channel, len(f.channels))
	for i, c := range f.channels {
		cp := *c
		out[i] = &cp
	}
	return out, nil
}

func (f *fakeAPI) GuildChannelCreateComplex(_ string, d discordgo.GuildChannelCreateData, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.Type == discordgo.ChannelTypeGuildNews && f.noNews {
		return nil, fmt.Errorf("community required")
	}
	c := &discordgo.Channel{ID: f.id(), Name: d.Name, Type: d.Type, ParentID: d.ParentID, Topic: d.Topic}
	f.channels = append(f.channels, c)
	cp := *c
	return &cp, nil
}

func (f *fakeAPI) ChannelEdit(id string, d *discordgo.ChannelEdit, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.channel(id)
	if c == nil {
		return nil, notFound()
	}
	if d.Name != "" {
		c.Name = d.Name
	}
	if d.ParentID != "" {
		c.ParentID = d.ParentID
	}
	cp := *c
	return &cp, nil
}

func (f *fakeAPI) GuildRoles(string, ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	return f.roles, nil
}
