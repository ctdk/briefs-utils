PREFIX ?= /usr/local
BINDIR = $(PREFIX)/bin
# mount(8) only searches for type helpers in /sbin and /usr/sbin (not
# /usr/local/sbin), so the mount.fuse.briefs helper defaults to /usr/sbin;
# override SBINDIR for packaging.
SBINDIR ?= /usr/sbin
MANDIR = $(PREFIX)/share/man

.PHONY: all generate mkfs fsck fuse man install install-man clean

all: mkfs fsck fuse

generate:
	go generate ./briefs

mkfs: generate cmd/mkfs/mkfs.go
	go build -o mkfs.briefs ./cmd/mkfs

fsck: generate cmd/fsck/fsck.go
	go build -o fsck.briefs ./cmd/fsck

fuse: generate cmd/fuse/fuse.go
	go build -o fuse.briefs ./cmd/fuse

man: all
	./mkfs.briefs --generate-man-page
	./fsck.briefs --generate-man-page
	./fuse.briefs --generate-man-page

install: all
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 mkfs.briefs $(DESTDIR)$(BINDIR)/mkfs.briefs
	install -m 0755 fsck.briefs $(DESTDIR)$(BINDIR)/fsck.briefs
	install -m 0755 fuse.briefs $(DESTDIR)$(BINDIR)/fuse.briefs
	install -d $(DESTDIR)$(SBINDIR)
	install -m 0755 mount.fuse.briefs $(DESTDIR)$(SBINDIR)/mount.fuse.briefs

install-man: man
	install -d $(DESTDIR)$(MANDIR)/man8
	install -m 0644 man/man8/mkfs.briefs.8 $(DESTDIR)$(MANDIR)/man8/mkfs.briefs.8
	install -m 0644 man/man8/fsck.briefs.8 $(DESTDIR)$(MANDIR)/man8/fsck.briefs.8
	install -m 0644 man/man8/fuse.briefs.8 $(DESTDIR)$(MANDIR)/man8/fuse.briefs.8

clean:
	rm -f mkfs.briefs fsck.briefs fuse.briefs
