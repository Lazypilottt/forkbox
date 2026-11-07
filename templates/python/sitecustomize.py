# Forkbox: reseed user-space random generators after a sandbox is forked or
# restored (see forkbox_guest.py). FORKBOX_CLONE_HOOKS=0 turns it off.
import os

if os.environ.get("FORKBOX_CLONE_HOOKS", "1") != "0" and os.path.isdir("/run/forkbox"):
    try:
        import forkbox_guest

        forkbox_guest.install()
    except Exception:
        pass
