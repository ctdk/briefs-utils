PREFIX ?= /usr/local
BINDIR = $(PREFIX)/bin

.PHONY: all mkfs fsck fuse install clean

all: mkfs fsck fuse

mkfs: cmd/mkfs/mkfs.go
	go build -o mkfs.briefs ./cmd/mkfs

fsck: cmd/fsck/fsck.go
	go build -o fsck.briefs ./cmd/fsck

fuse: cmd/fuse/fuse.go
	go build -o fuse.briefs ./cmd/fuse

install: all
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 mkfs.briefs $(DESTDIR)$(BINDIR)/mkfs.briefs
	install -m 0755 fsck.briefs $(DESTDIR)$(BINDIR)/fsck.briefs
	install -m 0755 fuse.briefs $(DESTDIR)$(BINDIR)/fuse.briefs

clean:
	rm -f mkfs.briefs fsck.briefs fuse.briefs
