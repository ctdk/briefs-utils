#!/bin/bash
# Test that mkfs.briefs and fsck.briefs reject mounted filesystems

set -e

cd /home/jeremy/go/src/github.com/ctdk/briefs-utils

# Create a test image
./mkfs.briefs -o /tmp/test-mount-check.img --size 8192 2>/dev/null

# Create a mount point
mkdir -p /tmp/testmnt
mount -o loop /tmp/test-mount-check.img /tmp/testmnt 2>&1 || true

# Give it a moment
sleep 1

# Try to run mkfs on the mounted image
echo "Testing mkfs.briefs on mounted image..."
if ./mkfs.briefs -o /tmp/test-mount-check.img 2>&1 | grep -q "refusing to create filesystem"; then
    echo "  PASS: mkfs.briefs rejected mounted filesystem"
else
    echo "  FAIL: mkfs.briefs did not reject mounted filesystem"
    echo "  Output was:"
    ./mkfs.briefs -o /tmp/test-mount-check.img 2>&1 || true
fi

# Try to run fsck on the mounted image
echo "Testing fsck.briefs on mounted image..."
if ./fsck.briefs -d /tmp/test-mount-check.img 2>&1 | grep -q "refusing to check filesystem"; then
    echo "  PASS: fsck.briefs rejected mounted filesystem"
else
    echo "  FAIL: fsck.briefs did not reject mounted filesystem"
    echo "  Output was:"
    ./fsck.briefs -d /tmp/test-mount-check.img 2>&1 || true
fi

# Clean up
umount /tmp/testmnt 2>/dev/null || true
rm -f /tmp/test-mount-check.img
rmdir /tmp/testmnt 2>/dev/null || true
