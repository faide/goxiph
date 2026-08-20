package ogg

// A Packet is one codec-level unit carried by the container.
//
// GranulePos is opaque here. Ogg does not know that it counts samples in Vorbis,
// samples plus pre-skip in Opus, or frames in FLAC; only the codec interprets it.
type Packet struct {
	Data       []byte
	GranulePos int64  // NoGranule unless this packet is the last to end on its page
	Serial     uint32 // logical stream this packet belongs to
	FirstPage  bool   // packet began on a page carrying FlagBOS
	LastPage   bool   // packet ended on a page carrying FlagEOS
}
