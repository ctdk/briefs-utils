PREFIX ?= /usr/local
BINDIR = $(PREFIX)/bin

.PHONY: all generate mkfs fsck fuse install clean

all: mkfs fsck fuse

generate:
	go generate ./briefs

mkfs: generate cmd/mkfs/mkfs.go
	go build -o mkfs.briefs ./cmd/mkfs

fsck: generate cmd/fsck/fsck.go
	go build -o fsck.briefs ./cmd/fsck

fuse: generate cmd/fuse/fuse.go
	go build -o fuse.briefs ./cmd/fuse

install: all
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 mkfs.briefs $(DESTDIR)$(BINDIR)/mkfs.briefs
	install -m 0755 fsck.briefs $(DESTDIR)$(BINDIR)/fsck.briefs
	install -m 0755 fuse.briefs $(DESTDIR)$(BINDIR)/fuse.briefs

clean:
	rm -f mkfs.briefs fsck.briefs fuse.briefs
