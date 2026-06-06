PREFIX ?= /usr/local
BINDIR = $(PREFIX)/bin

.PHONY: all mkfs fsck install clean

all: mkfs fsck

mkfs: cmd/mkfs/mkfs.go
	go build -o mkfs.briefs ./cmd/mkfs

fsck: cmd/fsck/fsck.go
	go build -o fsck.briefs ./cmd/fsck

install: all
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 mkfs.briefs $(DESTDIR)$(BINDIR)/mkfs.briefs
	install -m 0755 fsck.briefs $(DESTDIR)$(BINDIR)/fsck.briefs

clean:
	rm -f mkfs.briefs fsck.briefs
