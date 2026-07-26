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
export function resizeImage(file, maxEdge = MAX_EDGE_PX) {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => {
      URL.revokeObjectURL(url);
      const scale = Math.min(1, maxEdge / Math.max(img.width, img.height));
      const w = Math.max(1, Math.round(img.width * scale));
      const hgt = Math.max(1, Math.round(img.height * scale));
      const canvas = document.createElement('canvas');
      canvas.width = w;
      canvas.height = hgt;
      const ctx = canvas.getContext('2d');
      if (!ctx) { reject(new Error('canvas')); return; }
      ctx.drawImage(img, 0, 0, w, hgt);
      canvas.toBlob((blob) => {
        if (blob) resolve(blob);
        else reject(new Error('encode'));
      }, 'image/jpeg', 0.85);
    };
    img.onerror = () => { URL.revokeObjectURL(url); reject(new Error('decode')); };
    img.src = url;
  });
}
