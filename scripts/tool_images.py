"""Read a verified, immutable preset image selection; never update it here."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

def selection(profile, context, root=ROOT):
    manifest_path = root / '.esa/tool-images/current.json'
    if not manifest_path.exists():
        raise RuntimeError('Preset images are not prepared; run scripts/update_tool_images.py first')
    manifest = json.loads(manifest_path.read_text())
    if manifest.get('schema_version') != 1 or manifest.get('docker_context') != context:
        raise RuntimeError('Preset image registry does not match this Docker context')
    if profile not in manifest.get('profiles', []):
        raise RuntimeError('Profile has no verified immutable image')
    image = manifest.get('toolkit')
    if not image or not image['image_id'].startswith('sha256:'):
        raise RuntimeError('Shared toolkit is not prepared; run scripts/update_tool_images.py')
    return image, manifest
