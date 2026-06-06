"""MicroPython boot hook — runs before main.py on every reset."""
import gc

gc.collect()

try:
    import esp

    esp.osdebug(None)
except ImportError:
    pass
