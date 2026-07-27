// Client-side image preparation, shared by the two things that upload one: a
// person's avatar and a calendar's picture.
//
// The server re-encodes whatever arrives to a 128px JPEG anyway. Downscaling here
// first is about the wire, not the result: a modern phone photo is several
// megabytes, and the upload limit is one.

export const MAX_UPLOAD_BYTES = 1024 * 1024;      // the server's ceiling
export const MAX_SOURCE_BYTES = 20 * 1024 * 1024; // refuse to decode a RAW-sized file
export const MAX_EDGE_PX = 256;

/**
 * Downscale a picked File to a JPEG blob of at most MAX_EDGE_PX on its long edge.
 * Rejects when the browser cannot decode the file, which is also how a renamed
 * non-image gets caught before it reaches the network.
 */
export async function resizeImage(file, maxEdge = MAX_EDGE_PX) {
  const source = await decode(file);
  const scale = Math.min(1, maxEdge / Math.max(source.width, source.height));
  const w = Math.max(1, Math.round(source.width * scale));
  const hgt = Math.max(1, Math.round(source.height * scale));

  const canvas = document.createElement('canvas');
  canvas.width = w;
  canvas.height = hgt;
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('canvas');
  ctx.drawImage(source, 0, 0, w, hgt);
  if (source.close) source.close();

  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob);
      else reject(new Error('encode'));
    }, 'image/jpeg', 0.85);
  });
}

/**
 * Decode a picked file to something drawable.
 *
 * createImageBitmap takes the Blob directly, which matters: the obvious approach —
 * URL.createObjectURL then img.src — asks the page to load a blob: URL, and this
 * app's own Content-Security-Policy allows img-src from 'self' and data: only. That
 * made every avatar and calendar picture fail to upload, with the error surfacing as
 * a decode failure long before any request was made.
 */
async function decode(file) {
  if (typeof createImageBitmap === 'function') {
    return createImageBitmap(file);
  }
  // Older Safari: a data: URL is allowed by the same policy.
  const dataURL = await new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(new Error('read'));
    reader.readAsDataURL(file);
  });
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('decode'));
    img.src = dataURL;
  });
}
