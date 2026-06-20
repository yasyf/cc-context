from __future__ import annotations

import click
from loguru import logger


@click.group()
@click.version_option(package_name="cc-context")
def main() -> None:
    """Tools & skills for keeping Claude's context minimal."""


@main.command()
def hello() -> None:
    """Print a greeting — the starter command."""
    logger.debug("hello invoked")
    click.echo("Hello from cc-context!")
