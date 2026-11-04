#!/usr/bin/env python3
"""Verify the final MF timeline and actual sticker pixels (no Pillow dependency)."""
import json
import math
import struct
import subprocess
import sys
from pathlib import Path


def require(value, message):
    if not value:
        raise RuntimeError(message)


def image(path):
    header = path.read_bytes()[:29]
    require(header[:8] == b'\x89PNG\r\n\x1a\n', f'not PNG: {path}')
    width, height = struct.unpack('>II', header[16:24])
    depth, color = header[24:26]
    # Avoid swscale's 8->16 bit rescaling: use native 8-bit RGBA directly.
    # For Go RGB16 source PNGs, preserve each native sample's high byte.
    fmt = ('rgb48be' if color == 2 else 'rgba64be') if depth == 16 else 'rgba'
    data = subprocess.run(['ffmpeg', '-v', 'error', '-threads', '1', '-i', str(path),
                           '-threads', '1', '-f', 'rawvideo', '-pix_fmt', fmt, '-'],
                          check=True, stdout=subprocess.PIPE).stdout
    if depth == 16:
        data = data[::2]
        if color == 2:
            data = b''.join(data[i:i + 3] + b'\xff' for i in range(0, len(data), 3))
    require(len(data) == width * height * 4, 'unexpected decoded image length')
    return width, height, data



def resized(im, width, height):
    sw, sh, pixels = im
    out = bytearray(width * height * 4)
    for y in range(height):
        for x in range(width):
            src = ((y * sh // height) * sw + x * sw // width) * 4
            dst = (y * width + x) * 4
            out[dst:dst + 4] = pixels[src:src + 4]
    return out


def reaction_key(gesture, hand):
    return (gesture['recognizer_epoch'], hand['track_id'], hand['active_reaction']['event_id'])


def expected_hands(selected, frame, deadline, mode, max_age, arrivals, event_arrivals):
    if not selected:
        return {}
    age = ((frame['rtp_timestamp'] - selected['rtp_timestamp']) & 0xFFFFFFFF) // 90
    if mode == 'arrival':
        fresh = 0 <= deadline - arrivals[selected['response_id']] <= max_age
    else:
        fresh = age <= max_age
    if not fresh:
        return {}
    now = selected['timestamp_ms'] + age
    result = {}
    for hand in selected['hands']:
        a = hand.get('active_reaction')
        if not a:
            continue
        if mode == 'arrival':
            first = event_arrivals[reaction_key(selected, hand)]
            active = 0 <= deadline - first < a['valid_until_ms'] - a['started_at_ms']
        else:
            active = a['started_at_ms'] <= now < a['valid_until_ms']
        if active:
            result[hand['track_id']] = hand
    return result


def validate(run, assets):
    load = lambda p: json.loads(p.read_text())
    state = load(run / 'callbacks.json')
    result = load(run / 'result.json')
    mode = result.get('gesture_render_mode', 'realtime')
    require(mode in ('realtime', 'arrival', 'source'), 'unknown gesture render mode')
    report = {}
    epochs = set()
    segmentation = result.get('segmentation_enabled', True)
    scripted = result.get('gesture_mode', 'mock') == 'mock'
    require_reaction = result.get('require_gesture', False)
    for leg in ('tCore', 'tAccess'):
        folder = run / 'output' / leg.lower()
        frames = load(folder / 'frames.json')
        mapping = load(folder / 'render.json')
        require(len(frames) == len(mapping), f'{leg}: incomplete timeline')
        index = {f['rtp_timestamp']: i for i, f in enumerate(frames)}
        callbacks = [c for c in state['callbacks'] if c['path'] == f'/results/{leg}']
        gestures = [c['gesture_result'] for c in callbacks if c['kind'] == 'video_gesture']
        masks = {c['video_mask']['frame_id']: c['video_mask'] for c in callbacks if c['kind'] == 'video_mask'}
        require(gestures, f'{leg}: no gesture callbacks')
        if not segmentation:
            require(not masks and all(c['kind'] == 'video_gesture' for c in callbacks), f'{leg}: unexpected non-gesture callback')
            require(result['background'] == 'source', 'gesture-only must preserve source video')
            require(all(row['mask_frame_id'] == 0 and not row['exact_mask'] for row in mapping), f'{leg}: unexpected mask in gesture-only output')
        require(len({g['response_id'] for g in gestures}) == len(gestures), f'{leg}: duplicate response IDs')
        require(len({g['recognizer_epoch'] for g in gestures}) == 1, f'{leg}: unexpected task restart')
        epochs.add(gestures[0]['recognizer_epoch'])
        realtime = result.get('realtime_playout')
        gesture_age = realtime['legs'][leg]['gesture_max_age_ms'] if realtime else 500
        timeline = sorted((index[g['rtp_timestamp']], g) for g in gestures)
        arrivals = {c['gesture_result']['response_id']: c.get('render', {}).get('accepted_at_ms', 0)
                    for c in callbacks if c['kind'] == 'video_gesture'}
        event_arrivals = {}
        for g in gestures:
            for hand in g['hands']:
                if hand.get('active_reaction'):
                    key = reaction_key(g, hand)
                    event_arrivals[key] = min(event_arrivals.get(key, arrivals[g['response_id']]), arrivals[g['response_id']])
        multi = sum(len(g['hands']) >= 2 for g in gestures)
        if scripted:
            require(multi, f'{leg}: multi-hand transport not exercised')
        centers = [h['center'][0] for g in gestures for h in g['hands'] if h['track_id'] == 1]
        if scripted:
            require(centers and max(centers) - min(centers) > 0.02, f'{leg}: hand coordinates did not move')
        if scripted and result['duration_sec'] >= 2.8:
            require(any(not g['hands'] for g in gestures), f'{leg}: missing clear-hands snapshot')
        selected = None
        cursor = 0
        pixel_sample = None
        for i, row in enumerate(mapping):
            while cursor < len(timeline) and timeline[cursor][0] <= i:
                selected = timeline[cursor][1]
                cursor += 1
            if realtime:
                if row.get('dropped'):
                    require(i > 0 and row['file'] == mapping[i-1]['file'] and row['stickers'] == mapping[i-1]['stickers'],
                            f'{leg}/{i}: dropped frame changed emitted pixels')
                    continue
                eligible = [g for idx, g in timeline if idx <= i and (mode == 'source' or arrivals[g['response_id']] <= row['deadline_ms'])]
                selected = max(eligible, key=lambda g: index[g['rtp_timestamp']], default=None)
                require(row['gesture_response_id'] == (selected['response_id'] if selected else ''),
                        f'{leg}/{i}: gesture was not available at playout deadline')
            expected = expected_hands(selected, frames[i], row['deadline_ms'], mode, gesture_age, arrivals, event_arrivals)
            trace = row.get('gesture_trace', {})
            require(trace.get('mode') == mode, f'{leg}/{i}: trace mode mismatch')
            if selected:
                source_ms = math.floor(frames[index[selected['rtp_timestamp']]]['time_sec'] * 1000 + 0.5)
                require(trace.get('source_media_ms') == source_ms and
                        trace.get('accepted_at_ms') == arrivals[selected['response_id']] and
                        trace.get('receive_lag_ms') == arrivals[selected['response_id']] - source_ms and
                        trace.get('display_lag_ms') == math.floor(frames[i]['time_sec'] * 1000 + 0.5) - source_ms,
                        f'{leg}/{i}: incorrect gesture latency trace')
            stickers = row['stickers']
            require({s['track_id'] for s in stickers} == set(expected), f'{leg}/{i}: future, stale or missing sticker')
            for s in stickers:
                hand = expected[s['track_id']]
                require(s['emoji_key'] == hand['active_reaction']['emoji_key'], 'wrong emoji asset')
                require(s['event_first_received_at_ms'] == event_arrivals[reaction_key(selected, hand)], 'reaction restarted on repeated snapshot')
                w, h = frames[i]['width'], frames[i]['height']
                require(0 <= s['x'] <= w - s['width'] and 0 <= s['y'] <= h - s['height'], 'sticker out of frame')
                require(abs(s['x'] + s['width'] / 2 - hand['center'][0] * w) <= s['width'], 'sticker does not follow hand')
            if pixel_sample is None and len(stickers) >= (2 if scripted else 1):
                pixel_sample = i
        if scripted or require_reaction:
            require(pixel_sample is not None, f'{leg}: no required reaction rendered')
        if pixel_sample is None:
            pixel_sample = len(mapping) // 2
        i = pixel_sample
        row = mapping[i]
        w, h, actual = image(folder / row['file'])
        source = image(folder / frames[row.get('source_index', i)]['file'])
        background = result['background']
        if not segmentation or not row['mask_frame_id']:
            base = bytearray(source[2])
        else:
            base = resized(image(Path(background)), w, h) if background != 'black' else bytearray([0, 0, 0, 255] * (w * h))
        mask_id = row['mask_frame_id']
        if mask_id:
            mask = resized(image(folder / 'masks' / f'{mask_id:06d}.png'), w, h)
            for p in range(w * h):
                if mask[p * 4]:
                    base[p * 4:p * 4 + 4] = source[2][p * 4:p * 4 + 4]
        expected = bytearray(base)
        for sticker in row['stickers']:
            sw, sh = sticker['width'], sticker['height']
            pixels = resized(image(assets / (sticker['emoji_key'] + '.png')), sw, sh)
            for y in range(sh):
                for x in range(sw):
                    src = (y * sw + x) * 4
                    dst = ((sticker['y'] + y) * w + sticker['x'] + x) * 4
                    a = pixels[src + 3]
                    for c in range(3):
                        expected[dst + c] = (pixels[src + c] * a + expected[dst + c] * (255 - a) + 127) // 255
                    expected[dst + 3] = a + (expected[dst + 3] * (255 - a) + 127) // 255
        require(actual == bytes(expected), f'{leg}: rendered pixels disagree with source/background + RGBA stickers')
        changed = sum(actual[p:p + 4] != base[p:p + 4] for p in range(0, len(actual), 4))
        if row['stickers']:
            require(changed > 0, f'{leg}: stickers did not change output pixels')
        report[leg] = dict(gesture_render_mode=mode, snapshots=len(gestures), multi_hand_snapshots=multi, source_frames=len(frames),
                           pixel_sample=row['file'], changed_pixels=changed, pixels_verified=True, reaction_rendered=any(r['stickers'] for r in mapping), segmentation_enabled=segmentation,
                           source_preserved=not segmentation,
                           emoji_keys=sorted({s['emoji_key'] for r in mapping for s in r['stickers']}))
    require(len(epochs) == 2, 'two legs share recognizer state')
    result['gesture_validation'] = report
    (run / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Gesture timeline and exact RGBA pixels: PASS', json.dumps(report))


if __name__ == '__main__':
    try:
        validate(Path(sys.argv[1]), Path(sys.argv[2]))
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.CalledProcessError) as exc:
        print(f'Gesture validation failed: {exc}', file=sys.stderr)
        sys.exit(1)
