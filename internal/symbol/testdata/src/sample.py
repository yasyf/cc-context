"""Sample module."""


class Widget:
    """A widget.

    Second paragraph of the class doc.
    """

    def render(self):
        """Draw it."""
        return self.name

    def build(self):
        return Widget()


def helper():
    # a leading comment
    return render_all()


def render_all():
    return 1


MAX = 10
